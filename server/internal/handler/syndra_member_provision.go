package handler

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mozillazg/go-pinyin"
	"github.com/multica-ai/multica/server/internal/auth"
	enterpriseLark "github.com/multica-ai/multica/server/internal/enterprise/lark"
	"github.com/multica-ai/multica/server/internal/externalissue"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

var errSyndraIdentityConflict = errors.New("conflicting or ambiguous Feishu account; manual review required")

func (h *Handler) configureSyndraProvisioning(i *externalissue.Importer) {
	// Only the dedicated Bug endpoint can install this callback. Neither the
	// destination workspace nor the directory URL is supplied by the webhook.
	cfg := enterpriseLark.ConfigFromEnv()
	if !cfg.Enabled || cfg.AppID == "" || cfg.AppSecret == "" {
		return
	}
	i.Config.LarkAppID, i.Config.LarkAppSecret = cfg.AppID, cfg.AppSecret
	i.ProvisionBugMember = func(ctx context.Context, workspaceID pgtype.UUID, payload externalissue.BugSyncPayload, item externalissue.BugSyncItem) (db.Member, string, error) {
		p, err := i.ResolveBugDirectoryProfile(ctx, payload, item)
		if err != nil {
			return db.Member{}, "", err
		}
		if (cfg.DefaultTenant != "" && cfg.DefaultTenant != p.TenantKey) || (len(cfg.TenantAllowlist) > 0 && !contains(cfg.TenantAllowlist, p.TenantKey)) {
			return db.Member{}, "", errors.New("Feishu login tenant is not enabled for provisioning")
		}
		member, err := h.admitSyndraBugMember(ctx, workspaceID, p)
		candidate := ""
		if err == nil && p.Email == "" {
			candidate = syndraEmailCandidate(p.Name)
		}
		return member, candidate, err
	}
}

func syndraEmailCandidate(name string) string {
	a := pinyin.NewArgs()
	a.Fallback = func(r rune, _ pinyin.Args) []string {
		if unicode.IsSpace(r) {
			return nil
		}
		return []string{strings.ToLower(string(r))}
	}
	local := strings.Join(pinyin.LazyPinyin(name, a), "")
	if local == "" || len(local) > 64 || strings.IndexFunc(local, func(r rune) bool { return !(r >= 'a' && r <= 'z') }) != -1 {
		return ""
	}
	return local + "@qtshe.com"
}

// lookupSyndraLoginAccount considers only the directory's authoritative
// enterprise email and bound Feishu IDs. The pinyin candidate is never input.
func lookupSyndraLoginAccount(ctx context.Context, q *db.Queries, p enterpriseLark.Profile) (user db.User, bound bool, err error) {
	if p.Email != "" {
		user, err = q.GetUserByEmail(ctx, p.Email)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return db.User{}, false, err
		}
	}
	bindings, err := q.ListSyndraLoginIdentities(ctx, db.ListSyndraLoginIdentitiesParams{
		TenantKey: p.TenantKey, ExternalUserID: p.ExternalUserID, OpenID: p.OpenID, UnionID: p.UnionID, EmailUserID: user.ID,
	})
	if err != nil {
		return db.User{}, false, err
	}
	switch len(bindings) {
	case 0:
		return user, false, nil
	case 1:
		x := bindings[0]
		if x.TenantKey != p.TenantKey || x.OpenID.String != p.OpenID ||
			(x.UnionID.Valid && p.UnionID != "" && x.UnionID.String != p.UnionID) ||
			(x.ExternalUserID.Valid && x.ExternalUserID.String != p.ExternalUserID) ||
			(user.ID.Valid && user.ID != x.UserID) {
			return db.User{}, false, errSyndraIdentityConflict
		}
		user, err = q.GetUser(ctx, x.UserID)
		return user, true, err
	default:
		return db.User{}, false, errSyndraIdentityConflict
	}
}

func (h *Handler) admitSyndraBugMember(ctx context.Context, ws pgtype.UUID, p enterpriseLark.Profile) (db.Member, error) {
	var empty db.Member
	if p.Email != "" {
		address, err := mail.ParseAddress(p.Email)
		if err != nil || address.Address != p.Email || !strings.HasSuffix(p.Email, "@qtshe.com") {
			return empty, errSyndraIdentityConflict
		}
	}
	user, bound, err := lookupSyndraLoginAccount(ctx, h.Queries, p)
	if err != nil {
		return empty, err
	}
	alreadyMember := false
	if user.ID.Valid {
		if auth.IsTemporarilyDisabledUser(uuidToString(user.ID), user.Email) {
			return empty, auth.ErrTemporarilyDisabledUser
		}
		member, err := h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: user.ID, WorkspaceID: ws})
		if err == nil && bound {
			return member, nil
		}
		alreadyMember = err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
	}
	// Paid seat claims need a Cloud admission intent; self-hosted provisioning
	// must not synthesize an invitation or a share-link claim to bypass it.
	if !alreadyMember && h.seatCapacityEnabled() {
		return empty, errors.New("managed seat admission requires the native workspace flow")
	}
	email := larkEmail(p)
	if user.ID.Valid {
		email = user.Email
	}
	findPending := func(q *db.Queries, email string) (db.WorkspaceInvitation, error) {
		inv, err := q.GetPendingInvitationByEmail(ctx, db.GetPendingInvitationByEmailParams{WorkspaceID: ws, InviteeEmail: email})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.WorkspaceInvitation{}, nil
		}
		if err == nil && (inv.Role != "member" || len(inv.ProjectIds) > 0 || (inv.InviteeUserID.Valid && inv.InviteeUserID != user.ID)) {
			return db.WorkspaceInvitation{}, errors.New("pending invitation has a different admission scope")
		}
		return inv, err
	}
	var pending db.WorkspaceInvitation
	var limit memberLimit
	if !alreadyMember {
		pending, err = findPending(h.Queries, email)
		if err != nil {
			return empty, err
		}
		limit, err = h.memberLimitFor(ctx, ws, "member", pending.ID.Valid)
		if err != nil {
			return empty, err
		}
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	if err = q.LockWorkspaceMemberAdmission(ctx, ws); err != nil {
		return empty, err
	}
	user, bound, err = lookupSyndraLoginAccount(ctx, q, p)
	if err != nil {
		return empty, err
	}
	email = larkEmail(p)
	if auth.IsTemporarilyDisabledUserEmail(email) || (user.ID.Valid && auth.IsTemporarilyDisabledUser(uuidToString(user.ID), user.Email)) {
		return empty, auth.ErrTemporarilyDisabledUser
	}
	if user.ID.Valid {
		member, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: user.ID, WorkspaceID: ws})
		if err == nil {
			if !bound {
				if err := bindSyndraLoginAccount(ctx, q, user, p); err != nil {
					return empty, err
				}
				if err := tx.Commit(ctx); err != nil {
					return empty, err
				}
			}
			return member, nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
		email = user.Email
	}
	if alreadyMember {
		return empty, errors.New("workspace membership changed during identity check")
	}
	// Legacy branch confirmation is name-based. Do not create a second
	// workspace identity when a same-name member cannot be corroborated.
	members, err := q.ListMembersWithUser(ctx, ws)
	if err != nil {
		return empty, err
	}
	for _, m := range members {
		if strings.TrimSpace(m.UserName) == p.Name && m.UserID != user.ID {
			return empty, errSyndraIdentityConflict
		}
	}
	pending, err = findPending(q, email)
	if err != nil {
		return empty, err
	}
	limit.reserved = pending.ID.Valid
	admission, err := limit.admit(ctx, q)
	if err != nil {
		_ = tx.Rollback(ctx)
		admission.record(ctx)
		return empty, err
	}
	if !user.ID.Valid {
		// Use the same transaction for the signup policy's invitation read.
		// Parallel webhook admissions must not borrow a second DB connection.
		signup := Handler{cfg: h.cfg, Queries: q}
		if err = signup.checkSignupAllowed(ctx, email, true); err != nil {
			return empty, err
		}
		user, err = q.CreateUser(ctx, db.CreateUserParams{Name: p.Name, Email: email})
		if err != nil {
			return empty, err
		}
	}
	if !bound {
		err = bindSyndraLoginAccount(ctx, q, user, p)
		if err != nil {
			return empty, err
		}
	}
	if pending.ID.Valid {
		if _, err = q.AcceptInvitation(ctx, pending.ID); err != nil {
			return empty, err
		}
	}
	member, err := q.CreateMember(ctx, db.CreateMemberParams{UserID: user.ID, WorkspaceID: ws, Role: "member"})
	if err != nil {
		return empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	admission.record(ctx)
	if h.Bus != nil {
		h.publish(protocol.EventMemberAdded, uuidToString(ws), "system", "", map[string]any{"member": h.memberWithUserResponse(member, user), "source": "syndra_bug"})
		if pending.ID.Valid {
			h.publish(protocol.EventInvitationAccepted, uuidToString(ws), "system", "", map[string]any{"invitation_id": uuidToString(pending.ID), "member": h.memberWithUserResponse(member, user)})
		}
	}
	h.notifyDaemonWorkspacesChanged(uuidToString(user.ID))
	return member, nil
}

func bindSyndraLoginAccount(ctx context.Context, q *db.Queries, user db.User, p enterpriseLark.Profile) error {
	return q.BindSyndraLoginIdentity(ctx, db.BindSyndraLoginIdentityParams{UserID: user.ID, TenantKey: p.TenantKey,
		ExternalUserID: strToText(p.ExternalUserID), OpenID: strToText(p.OpenID), UnionID: strToText(p.UnionID),
		Email: strToText(p.Email), Name: strToText(p.Name), RawProfile: []byte(`{"source":"syndra_bug"}`)})
}
