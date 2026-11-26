package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	enterpriseLark "github.com/multica-ai/multica/server/internal/enterprise/lark"
	"github.com/multica-ai/multica/server/internal/entitlement"
	"github.com/multica-ai/multica/server/internal/externalissue"
)

func TestSyndraDirectoryProvisionAndLogin(t *testing.T) {
	ctx := t.Context()
	ws := memberLimitWorkspace(t, 0)
	nonce := uuid.NewString()
	letters := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return 'a' + r - '0'
		}
		return r
	}, nonce[:8])
	p := enterpriseLark.Profile{Name: "王凝" + letters, TenantKey: "tenant-" + nonce, ExternalUserID: "uid-" + nonce, OpenID: "ou_" + nonce, UnionID: "on_" + nonce}
	// A real account has the exact guessed address. It must neither receive
	// the task nor acquire a membership/Feishu binding.
	candidate := "wangning" + letters + "@qtshe.com"
	guessed := dbfx.User(t, "Holds candidate email", candidate)
	dbfx.Cleanup(t, `DELETE FROM "user" WHERE email=$1`, larkEmail(p))
	h := &Handler{Queries: testHandler.Queries, TxStarter: testPool, IssueService: testHandler.IssueService, Bus: testHandler.Bus, cfg: Config{AllowSignup: true}}
	var frozen, wrongTenant, badProof atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/open/multica/version-bugs/assignee-identity":
			if r.Host != "test-syndra.lggj.work" || r.Header.Get("Authorization") != "Bearer 258023950a475b7ff9001a77612100c7f96cd2630d5d0c3719177d50b55e3f0a" {
				t.Error("wrong resolver token")
			}
			var ids struct {
				Bug  int64  `json:"bug_id"`
				Mate int64  `json:"mate_id"`
				Env  string `json:"source_env"`
			}
			if err := json.NewDecoder(r.Body).Decode(&ids); err != nil || ids.Bug != 42 || ids.Mate != 17 || ids.Env != "test" {
				t.Error("wrong resolver request")
			}
			if !badProof.Load() {
				mac := hmac.New(sha256.New, []byte("webhook"))
				mac.Write([]byte(strings.Join([]string{"multica:syndra:bug-assignee:v1", "response", "test", "42", "17", p.TenantKey, p.ExternalUserID, p.Name}, "\x00")))
				w.Header().Set("X-Multica-Directory-Proof", hex.EncodeToString(mac.Sum(nil)))
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 4000, "success": true, "data": map[string]any{"bug_id": 42, "mate_id": 17, "source_env": "test", "user_id": p.ExternalUserID, "tenant_key": p.TenantKey, "name": p.Name}})
		case "/open-apis/auth/v3/tenant_access_token/internal":
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "directory-token", "expire": 7200})
		case "/open-apis/tenant/v2/tenant/query":
			tenant := p.TenantKey
			if wrongTenant.Load() {
				tenant = "other-tenant"
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"tenant": map[string]any{"tenant_key": tenant}}})
		case "/open-apis/contact/v3/users/" + p.ExternalUserID:
			if r.Header.Get("Authorization") != "Bearer directory-token" || r.URL.Query().Get("user_id_type") != "user_id" {
				t.Error("wrong contact request")
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"user": map[string]any{"name": p.Name, "user_id": p.ExternalUserID, "open_id": p.OpenID, "union_id": p.UnionID, "status": map[string]bool{"is_activated": true, "is_resigned": false, "is_frozen": frozen.Load()}}}})
		default:
			t.Errorf("unexpected URL %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("LARK_AUTH_ENABLED", "true")
	t.Setenv("LARK_TENANT_KEY", "")
	t.Setenv("LARK_TENANT_ALLOWLIST", "")
	t.Setenv("LARK_APP_ID", "login-app")
	t.Setenv("LARK_APP_SECRET", "test-secret")
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = "127.0.0.1"
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "test-syndra.lggj.work:443" && addr != srv.Listener.Addr().String() {
			return nil, errors.New("unexpected test destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	defer tr.CloseIdleConnections()
	i := externalissue.Importer{Queries: h.Queries, IssueService: h.IssueService, HTTPClient: &http.Client{Transport: tr}, Config: externalissue.Config{WebhookToken: "webhook", BugWorkspaceID: ws, LarkOpenAPIBaseURL: srv.URL}}
	h.configureSyndraProvisioning(&i)
	mate := int64(17)
	request := externalissue.BugSyncRequest{Payload: externalissue.BugSyncPayload{Source: "syndra", SourceEnv: "test", Items: []externalissue.BugSyncItem{{Event: "upsert", EntityType: "version_bug", ExternalKey: "syndra:test:version_bug:42", BugID: 42, Title: "directory test", Status: "active", Assignee: externalissue.BugSyncPerson{MateID: &mate, Name: &p.Name},
		Metadata: map[string]any{"bug_assignee_email_candidate": "forged@example.org"}}}}}
	badProof.Store(true)
	if _, err := i.ResolveBugDirectoryProfile(ctx, request.Payload, request.Payload.Items[0]); err == nil {
		t.Fatal("accepted unsigned lookup")
	}
	badProof.Store(false)
	wrongTenant.Store(true)
	if _, err := i.ResolveBugDirectoryProfile(ctx, request.Payload, request.Payload.Items[0]); err == nil {
		t.Fatal("accepted cross-tenant identity")
	}
	wrongTenant.Store(false)
	for _, key := range []string{"LARK_TENANT_KEY", "LARK_TENANT_ALLOWLIST"} {
		t.Setenv(key, "not-this-tenant")
		h.configureSyndraProvisioning(&i)
		if _, _, err := i.ProvisionBugMember(ctx, parseUUID(ws), request.Payload, request.Payload.Items[0]); err == nil {
			t.Fatalf("%s restriction bypassed", key)
		}
		t.Setenv(key, "")
	}
	h.configureSyndraProvisioning(&i)
	frozen.Store(true)
	res, err := i.ImportBugSync(ctx, request)
	if err != nil || res.Items[0].Issue.AssigneeID != parseUUID(testUserID) {
		t.Fatalf("inactive must remain with owner: %v", err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM "user" WHERE email=$1`, larkEmail(p)); n != 0 {
		t.Fatal("created frozen employee")
	}
	frozen.Store(false)
	res, err = i.ImportBugSync(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	issue := res.Items[0].Issue
	if !res.Items[0].Existing || issue.AssigneeID == parseUUID(guessed) || issue.AssigneeID == parseUUID(testUserID) {
		t.Fatal("not assigned on replay")
	}
	var meta map[string]any
	if json.Unmarshal(issue.Metadata, &meta) != nil || meta["bug_assignee_email_candidate"] != candidate {
		t.Fatalf("missing unverified candidate: %s", issue.Metadata)
	}
	user, isNew, err := h.findOrCreateLarkUser(httptest.NewRequest("POST", "/auth/lark", nil), p)
	if err != nil || isNew || user.ID != issue.AssigneeID || user.Email != larkEmail(p) {
		t.Fatalf("first login created or selected wrong account: %v", err)
	}
	res, err = i.ImportBugSync(ctx, request)
	if err != nil || res.Items[0].Issue.ID != issue.ID || res.Items[0].Issue.AssigneeID != user.ID {
		t.Fatalf("replay: %v", err)
	}
	meta = nil
	if json.Unmarshal(res.Items[0].Issue.Metadata, &meta) != nil || meta["bug_assignee_email_candidate"] != candidate {
		t.Fatal("lost candidate on replay")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM member WHERE workspace_id=$1 AND user_id=$2 AND role='member'`, ws, user.ID); n != 1 {
		t.Fatal("wrong membership")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM workspace_invitation WHERE workspace_id=$1`, ws); n != 0 {
		t.Fatal("created invitation")
	}
}

func TestSyndraProvisionMembershipCapacity(t *testing.T) {
	ws := memberLimitWorkspace(t, 0)
	id := uuid.NewString()
	p := enterpriseLark.Profile{Name: "已核验员工", TenantKey: "tenant-" + id, ExternalUserID: "uid-" + id, OpenID: "ou_" + id, Email: id + "@qtshe.com"}
	h := &Handler{Queries: testHandler.Queries, TxStarter: testPool, cfg: Config{AllowSignup: true}, Entitlements: useMemberLimit(t, ws, entitlement.ActionEnforce, 1)}
	if _, err := h.admitSyndraBugMember(t.Context(), parseUUID(ws), p); !errors.Is(err, errMemberLimitReached) {
		t.Fatalf("full workspace: %v", err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM "user" WHERE email=$1`, p.Email); n != 0 {
		t.Fatal("new user leaked after denied admission")
	}
	existing := dbfx.User(t, p.Name, p.Email)
	h.Entitlements = useMemberLimit(t, ws, entitlement.ActionEnforce, 2)
	inv := pendingInvitation(t, ws, p.Email, existing)
	m, err := h.admitSyndraBugMember(t.Context(), parseUUID(ws), p)
	if err != nil || m.UserID != parseUUID(existing) {
		t.Fatalf("reserved seat admission: %v", err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM workspace_invitation WHERE workspace_id=$1 AND id=$2 AND status='accepted'`, ws, inv); n != 1 {
		t.Fatal("old invitation still holds a slot")
	}
}

func TestSyndraProvisionVerifiedEmailReuseAndConflict(t *testing.T) {
	ws := parseUUID(memberLimitWorkspace(t, 0))
	id := uuid.NewString()
	p := enterpriseLark.Profile{Name: "目录成员", TenantKey: "tenant-" + id, ExternalUserID: "user-" + id, OpenID: "ou_" + id, Email: id + "@qtshe.com"}
	account := dbfx.User(t, p.Name, p.Email)
	h := &Handler{Queries: testHandler.Queries, TxStarter: testPool, cfg: Config{AllowSignup: false}}
	var wg sync.WaitGroup
	for n := 0; n < 3; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := h.admitSyndraBugMember(t.Context(), ws, p)
			if err != nil || m.UserID != parseUUID(account) {
				t.Errorf("reuse: %v", err)
			}
		}()
	}
	wg.Wait()
	p.OpenID = "ou_other"
	p.ExternalUserID = "different"
	if _, err := h.admitSyndraBugMember(t.Context(), ws, p); !errors.Is(err, errSyndraIdentityConflict) {
		t.Fatalf("rebound conflicting identity: %v", err)
	}
	p.Email = ""
	p.UnionID = "on_" + id
	if _, err := h.admitSyndraBugMember(t.Context(), parseUUID(memberLimitWorkspace(t, 0)), p); err == nil {
		t.Fatal("signup policy bypassed")
	}
	if _, err := h.Queries.GetUserByEmail(t.Context(), larkEmail(p)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unexpected partial account: %v", err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM user_external_identity WHERE user_id=$1 AND open_id=$2`, account, "ou_"+id); n != 1 {
		t.Fatal("original binding lost")
	}
}
