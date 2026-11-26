package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	enterpriseLark "github.com/multica-ai/multica/server/internal/enterprise/lark"
	"github.com/multica-ai/multica/server/internal/logger"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type LarkLoginRequest struct {
	Code        string `json:"code"`
	RedirectURI string `json:"redirect_uri"`
}

func (h *Handler) LarkLogin(w http.ResponseWriter, r *http.Request) {
	var req LarkLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}

	profile, err := enterpriseLark.NewOAuthClientFromEnv().ExchangeCode(r.Context(), req.Code, req.RedirectURI)
	if err != nil {
		status := http.StatusBadGateway
		switch err.Error() {
		case "Lark login is not enabled":
			status = http.StatusNotFound
		case "Lark login is not configured":
			status = http.StatusServiceUnavailable
		case "Lark tenant is not allowed":
			status = http.StatusForbidden
		}
		slog.Warn("lark login failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, status, err.Error())
		return
	}

	user, isNew, err := h.findOrCreateLarkUser(r, profile)
	if err != nil {
		var signupErr SignupError
		if errors.As(err, &signupErr) {
			writeError(w, http.StatusForbidden, signupErr.Error())
			return
		}
		slog.Error("failed to bind lark user", append(logger.RequestAttrs(r), "error", err, "open_id", profile.OpenID)...)
		writeError(w, http.StatusInternalServerError, "failed to log in with Lark")
		return
	}
	if isNew {
		evt := analytics.Signup(uuidToString(user.ID), user.Email, signupSourceFromRequest(r))
		evt.Properties["auth_method"] = "lark"
		obsmetrics.RecordEvent(h.Analytics, h.Metrics, evt)
	}
	user, err = h.completeLarkLoginOnboarding(r, user)
	if err != nil {
		slog.Warn("lark login failed to mark onboarding complete", append(logger.RequestAttrs(r), "error", err, "user_id", uuidToString(user.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to log in with Lark")
		return
	}

	tokenString, err := h.issueJWT(user)
	if err != nil {
		slog.Warn("lark login failed", append(logger.RequestAttrs(r), "error", err, "user_id", uuidToString(user.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	if err := auth.SetAuthCookies(w, tokenString); err != nil {
		slog.Warn("failed to set auth cookies", "error", err)
	}
	if h.CFSigner != nil {
		for _, cookie := range h.CFSigner.SignedCookies(time.Now().Add(30 * 24 * time.Hour)) {
			http.SetCookie(w, cookie)
		}
	}

	slog.Info("user logged in via lark", append(logger.RequestAttrs(r), "user_id", uuidToString(user.ID), "open_id", profile.OpenID)...)
	writeJSON(w, http.StatusOK, LoginResponse{
		Token: tokenString,
		User:  h.userToResponse(user),
	})
}

func (h *Handler) findOrCreateLarkUser(r *http.Request, profile enterpriseLark.Profile) (db.User, bool, error) {
	user, err := h.Queries.GetUserByExternalIdentity(r.Context(), db.GetUserByExternalIdentityParams{
		Provider:  enterpriseLark.ProviderName,
		TenantKey: profile.TenantKey,
		UnionID:   profile.UnionID,
		OpenID:    profile.OpenID,
	})
	isNew := false
	alreadyBound := err == nil
	if err != nil {
		if !isNotFound(err) {
			return db.User{}, false, err
		}
		email := larkEmail(profile)
		user, isNew, err = h.findOrCreateUser(r.Context(), email)
		if err != nil {
			return db.User{}, false, err
		}
	}

	if alreadyBound && strings.HasSuffix(user.Email, "@lark.local") && canFillLarkLoginEmail(user, profile) {
		updated, err := h.completeLarkPlaceholderProfile(r.Context(), user, profile)
		return updated, false, err
	}
	user, err = persistLarkLoginProfile(r.Context(), h.Queries, user, profile, alreadyBound)
	return user, isNew, err
}

func persistLarkLoginProfile(ctx context.Context, q *db.Queries, user db.User, profile enterpriseLark.Profile, alreadyBound bool) (db.User, error) {
	user, err := updateUserFromLarkProfile(ctx, q, user, profile)
	if err != nil {
		return db.User{}, err
	}
	bindingEmail := profile.Email
	if alreadyBound {
		// Invitations can resolve an external identity by its email. Do not
		// publish a contact address (or another account's mailbox) on this binding.
		bindingEmail = user.Email
		if strings.HasSuffix(bindingEmail, "@lark.local") {
			bindingEmail = ""
		}
	}

	raw := profile.Raw
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if _, err := q.UpsertUserExternalIdentityByOpenID(ctx, db.UpsertUserExternalIdentityByOpenIDParams{
		UserID:         user.ID,
		Provider:       enterpriseLark.ProviderName,
		TenantKey:      profile.TenantKey,
		ExternalUserID: strToText(profile.ExternalUserID),
		OpenID:         strToText(profile.OpenID),
		UnionID:        strToText(profile.UnionID),
		Email:          strToText(bindingEmail),
		Name:           strToText(profile.Name),
		AvatarUrl:      strToText(profile.AvatarURL),
		RawProfile:     raw,
	}); err != nil {
		return db.User{}, err
	}

	return user, nil
}

func canFillLarkLoginEmail(user db.User, profile enterpriseLark.Profile) bool {
	email := profile.EnterpriseEmail
	if email == "" || profile.OpenID == "" || profile.TenantKey == "" || strings.HasSuffix(email, "@lark.local") ||
		auth.IsTemporarilyDisabledUser(uuidToString(user.ID), user.Email) || auth.IsTemporarilyDisabledUserEmail(email) {
		return false
	}
	address, err := mail.ParseAddress(email)
	return err == nil && address.Address == email
}

func (h *Handler) completeLarkPlaceholderProfile(ctx context.Context, user db.User, profile enterpriseLark.Profile) (db.User, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.User{}, err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	updated, err := q.FillLarkPlaceholderEmail(ctx, db.FillLarkPlaceholderEmailParams{
		UserID: user.ID, PlaceholderEmail: user.Email, EnterpriseEmail: profile.EnterpriseEmail, TenantKey: profile.TenantKey, OpenID: profile.OpenID,
	})
	if isUniqueViolation(err) || isNotFound(err) {
		// Another account owns the address, or a concurrent login already
		// filled this account. Keep the identity; never look up a user by the new email.
		_ = tx.Rollback(ctx)
		current, err := h.Queries.GetUser(ctx, user.ID)
		if err != nil {
			return db.User{}, err
		}
		return persistLarkLoginProfile(ctx, h.Queries, current, profile, true)
	}
	if err != nil {
		return db.User{}, err
	}
	updated, err = persistLarkLoginProfile(ctx, q, updated, profile, true)
	if err != nil {
		return db.User{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return db.User{}, err
	}
	return updated, nil
}

func (h *Handler) completeLarkLoginOnboarding(r *http.Request, user db.User) (db.User, error) {
	if user.OnboardedAt.Valid {
		return user, nil
	}
	user, err := h.Queries.MarkUserOnboarded(r.Context(), user.ID)
	if err != nil {
		return user, err
	}
	onboardedAt := ""
	if user.OnboardedAt.Valid {
		onboardedAt = user.OnboardedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	obsmetrics.RecordEvent(h.Analytics, h.Metrics, analytics.OnboardingCompleted(
		uuidToString(user.ID),
		"",
		analytics.OnboardingPathUnknown,
		onboardedAt,
		user.CloudWaitlistEmail.Valid,
	))
	return user, nil
}

func updateUserFromLarkProfile(ctx context.Context, q *db.Queries, user db.User, profile enterpriseLark.Profile) (db.User, error) {
	name := strings.TrimSpace(profile.Name)
	avatarURL := strings.TrimSpace(profile.AvatarURL)
	if name == "" && avatarURL == "" {
		return user, nil
	}

	newName := user.Name
	newAvatar := user.AvatarUrl
	needsUpdate := false
	if name != "" && name != user.Name {
		newName = name
		needsUpdate = true
	}
	if avatarURL != "" && (!user.AvatarUrl.Valid || user.AvatarUrl.String != avatarURL) {
		newAvatar = pgtype.Text{String: avatarURL, Valid: true}
		needsUpdate = true
	}
	if !needsUpdate {
		return user, nil
	}
	updated, err := q.UpdateUser(ctx, db.UpdateUserParams{
		ID:        user.ID,
		Name:      newName,
		AvatarUrl: newAvatar,
	})
	if err != nil {
		return user, err
	}
	return updated, nil
}

func larkEmail(profile enterpriseLark.Profile) string {
	if profile.Email != "" {
		return strings.ToLower(strings.TrimSpace(profile.Email))
	}
	id := profile.UnionID
	if id == "" {
		id = profile.OpenID
	}
	replacer := strings.NewReplacer("@", "-", ":", "-", "/", "-", "\\", "-", " ", "-")
	return strings.ToLower(replacer.Replace(id)) + "@lark.local"
}
