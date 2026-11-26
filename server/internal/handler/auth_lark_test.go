package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	enterpriseLark "github.com/multica-ai/multica/server/internal/enterprise/lark"
)

func TestLarkLoginEnrichesProvisionedPlaceholder(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := t.Context()
	ws := parseUUID(memberLimitWorkspace(t, 0))
	suffix := uuid.NewString()
	p := enterpriseLark.Profile{TenantKey: "tenant-" + suffix, OpenID: "ou_" + suffix, UnionID: "on_" + suffix, ExternalUserID: "employee-" + suffix, Name: "预建员工"}
	h := &Handler{Queries: testHandler.Queries, TxStarter: testPool, cfg: Config{AllowSignup: true}}
	member, err := h.admitSyndraBugMember(ctx, ws, p)
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, `DELETE FROM "user" WHERE id=$1`, member.UserID)
	dbfx.Cleanup(t, "DELETE FROM user_external_identity WHERE user_id=$1", member.UserID)
	dbfx.Cleanup(t, "DELETE FROM member WHERE id=$1", member.ID)
	userID := uuidToString(member.UserID)
	placeholder := p.UnionID + "@lark.local"
	u, err := h.Queries.GetUser(ctx, member.UserID)
	if err != nil || u.Email != placeholder || u.AvatarUrl.Valid {
		t.Fatalf("provisioning changed: email=%s avatar=%v err=%v", u.Email, u.AvatarUrl.Valid, err)
	}
	taken := "taken-" + suffix + "@qtshe.com"
	other := dbfx.User(t, "其他人", taken)
	actual := "real-" + suffix + "@qtshe.com"
	const avatar1 = "https://example.test/avatar-1.png"
	const avatar2 = "https://example.test/avatar-2.png"
	t.Setenv("LARK_AUTH_ENABLED", "true")
	t.Setenv("LARK_TENANT_ALLOWLIST", p.TenantKey)
	t.Setenv("LARK_APP_ID", "app")
	t.Setenv("LARK_APP_SECRET", "test-secret")

	// If storing the identity profile fails, the email change must roll back too.
	bad := p
	bad.EnterpriseEmail = actual
	bad.AvatarURL = avatar2
	bad.Raw = json.RawMessage("{")
	if _, _, err := h.findOrCreateLarkUser(httptest.NewRequest("POST", "/", nil), bad); err == nil {
		t.Fatal("expected invalid raw profile write to fail")
	}
	unchanged, err := h.Queries.GetUser(ctx, member.UserID)
	if err != nil || unchanged.Email != placeholder || unchanged.AvatarUrl.Valid {
		t.Fatal("failed login partially updated account")
	}

	// Use the HTTP login endpoint so the response and persisted user must
	// agree, not just the OAuth profile or the identity metadata.
	login := func(enterprise, avatar, wantEmail, wantAvatar string) {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/token":
				json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-token"})
			case "/user_info":
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("missing user token")
				}
				json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"tenant_key": p.TenantKey, "open_id": p.OpenID, "union_id": p.UnionID, "user_id": p.ExternalUserID, "name": "登录员工", "email": taken, "enterprise_email": enterprise, "avatar_url": avatar}})
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()
		t.Setenv("LARK_TOKEN_URL", srv.URL+"/token")
		t.Setenv("LARK_USER_INFO_URL", srv.URL+"/user_info")
		w := httptest.NewRecorder()
		testHandler.LarkLogin(w, newRequest("POST", "/api/auth/lark", map[string]string{"code": "fixture-code"}))
		var response LoginResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Token == "" ||
			response.User.ID != userID || response.User.Email != wantEmail {
			t.Fatalf("login did not retain the expected account: HTTP %d", w.Code)
		}
		stored, err := h.Queries.GetUser(ctx, member.UserID)
		if err != nil || stored.Email != wantEmail || stored.Name != "登录员工" || stored.AvatarUrl.String != wantAvatar {
			t.Fatalf("unexpected stored profile: %s %s %s err=%v", stored.Email, stored.Name, stored.AvatarUrl.String, err)
		}
	}
	login("", avatar1, placeholder, avatar1)
	login("not an address", "", placeholder, avatar1)
	login(taken, "", placeholder, avatar1)
	if n := dbfx.Count(t, "SELECT count(*) FROM user_external_identity WHERE user_id=$1 AND email=$2", member.UserID, taken); n != 0 {
		t.Fatal("occupied address was recorded for invitation matching")
	}
	login("  REAL-"+suffix+"@QTSHE.COM ", avatar2, actual, avatar2)
	if n := dbfx.Count(t, "SELECT count(*) FROM user_external_identity WHERE user_id=$1 AND tenant_key=$2 AND open_id=$3 AND email=$4", member.UserID, p.TenantKey, p.OpenID, actual); n != 1 {
		t.Fatal("expected one bound login with the real mailbox")
	}
	login(actual, avatar2, actual, avatar2)
	login("later-"+suffix+"@qtshe.com", "", actual, avatar2)
	if n := dbfx.Count(t, "SELECT count(*) FROM user_external_identity WHERE user_id=$1 AND email=$2", member.UserID, actual); n != 1 {
		t.Fatal("a later login changed the invitation mailbox")
	}
	if n := dbfx.Count(t, "SELECT count(*) FROM member WHERE workspace_id=$1 AND user_id=$2 AND id=$3", ws, member.UserID, member.ID); n != 1 {
		t.Fatal("membership changed on login")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM "user" WHERE id=$1 AND email=$2 AND name='其他人'`, other, taken); n != 1 {
		t.Fatal("login modified the owner of a different mailbox")
	}
}

func TestConcurrentLarkPlaceholderLoginsCannotShareMailbox(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := t.Context()
	ws := parseUUID(memberLimitWorkspace(t, 0))
	h := &Handler{Queries: testHandler.Queries, TxStarter: testPool, cfg: Config{AllowSignup: true}}
	suffix := uuid.NewString()
	email := suffix + "@qtshe.com"
	type fixture struct {
		profile enterpriseLark.Profile
		id      string
	}
	var accounts []fixture
	for _, prefix := range []string{"a", "b"} {
		key := prefix + suffix
		p := enterpriseLark.Profile{TenantKey: "tenant-" + suffix, OpenID: "ou_" + key, UnionID: "on_" + key, Name: prefix}
		member, err := h.admitSyndraBugMember(ctx, ws, p)
		if err != nil {
			t.Fatal(err)
		}
		dbfx.Cleanup(t, `DELETE FROM "user" WHERE id=$1`, member.UserID)
		dbfx.Cleanup(t, "DELETE FROM user_external_identity WHERE user_id=$1", member.UserID)
		dbfx.Cleanup(t, "DELETE FROM member WHERE id=$1", member.ID)
		p.EnterpriseEmail = email
		accounts = append(accounts, fixture{p, uuidToString(member.UserID)})
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, a := range accounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			u, isNew, err := h.findOrCreateLarkUser(httptest.NewRequest("POST", "/", nil), a.profile)
			if err != nil || isNew || uuidToString(u.ID) != a.id {
				t.Errorf("concurrent login changed identity: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if dbfx.Count(t, `SELECT count(*) FROM "user" WHERE email=$1 AND id IN ($2,$3)`, email, accounts[0].id, accounts[1].id) != 1 ||
		dbfx.Count(t, `SELECT count(*) FROM "user" WHERE email LIKE '%@lark.local' AND id IN ($1,$2)`, accounts[0].id, accounts[1].id) != 1 ||
		dbfx.Count(t, "SELECT count(*) FROM user_external_identity WHERE email=$1 AND tenant_key=$2", email, accounts[0].profile.TenantKey) != 1 {
		t.Fatal("expected one mailbox owner, one placeholder, and one email-to-identity binding")
	}
}

func TestCompleteLarkLoginOnboardingMarksNewLarkUserOnboarded(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	const (
		email     = "lark-onboard-new@multica.test"
		tenantKey = "tenant-onboard-new"
		openID    = "ou_onboard_new"
		unionID   = "onboard-new-union"
	)
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, email)
	})
	_, _ = testPool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, email)

	req := httptest.NewRequest("POST", "/auth/lark", nil)
	user, isNew, err := testHandler.findOrCreateLarkUser(req, enterpriseLark.Profile{
		OpenID:    openID,
		UnionID:   unionID,
		TenantKey: tenantKey,
		Email:     email,
		Name:      "Lark Onboard New",
		Raw:       []byte(`{"source":"test"}`),
	})
	if err != nil {
		t.Fatalf("findOrCreateLarkUser: %v", err)
	}
	if !isNew {
		t.Fatal("expected a fresh Lark login user")
	}
	if user.OnboardedAt.Valid {
		t.Fatal("findOrCreateLarkUser should not mark onboarding by itself")
	}

	user, err = testHandler.completeLarkLoginOnboarding(req, user)
	if err != nil {
		t.Fatalf("completeLarkLoginOnboarding: %v", err)
	}
	if !user.OnboardedAt.Valid {
		t.Fatal("expected Lark login to mark onboarded_at")
	}

	var onboardedAt *time.Time
	if err := testPool.QueryRow(ctx,
		`SELECT onboarded_at FROM "user" WHERE id = $1`,
		user.ID,
	).Scan(&onboardedAt); err != nil {
		t.Fatalf("lookup user onboarding state: %v", err)
	}
	if onboardedAt == nil {
		t.Fatal("expected onboarded_at to be persisted")
	}
}
