package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	enterpriseLark "github.com/multica-ai/multica/server/internal/enterprise/lark"
	"github.com/multica-ai/multica/server/internal/externalissue"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestImportExternalIssueRequiresWebhookToken(t *testing.T) {
	t.Setenv("MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN", "")

	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/webhooks/external-issues", map[string]any{})
	testHandler.ImportExternalIssue(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

func TestImportExternalIssueRejectsInvalidBearerToken(t *testing.T) {
	t.Setenv("MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN", "secret")

	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/webhooks/external-issues", map[string]any{})
	req.Header.Set("Authorization", "Bearer wrong")
	testHandler.ImportExternalIssue(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

func TestImportExternalIssueBugSyncCreatesIssue(t *testing.T) {
	t.Setenv("MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN", "secret")
	externalUserID := "handler-bug-sync-user"
	seedExternalIssueImportIdentity(t, externalUserID)
	t.Setenv("MULTICA_EXTERNAL_ISSUE_DEFAULT_WORKSPACE_ID", testWorkspaceID)
	t.Setenv("MULTICA_EXTERNAL_ISSUE_DEFAULT_LARK_USER_ID", externalUserID)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/webhooks/external-issues?sync_type=bug",
		strings.NewReader(`{
			"schema_version": "syndra.multica.version_bug.webhook.v1",
			"event_type": "version_bug.changed",
			"event_id": "syndra:local:version_bug:frontend_debug:1081",
			"scene": "frontend_debug",
			"source": "syndra",
			"source_env": "local",
			"sent_at": "2026-06-30T10:50:25+08:00",
			"item_count": 1,
			"item_ids": "1081",
			"items": [{
				"event": "upsert",
				"entity_type": "version_bug",
				"external_key": "syndra:local:version_bug:handler-test-1081",
				"bug_id": 1081,
				"version_id": 163,
				"version_name": "v2.91.56-企业一体化项目看板",
				"demand_name": "-企业一体化项目看板",
				"role": "frontend",
				"title": "【生成报告】iOS 14.6/16.1 白屏",
				"description": "版本：v2.91.56<br><p>[步骤]</p><p>打开生成报告</p><br><p>[结果]</p><p>白屏</p>",
				"priority": "一般",
				"bug_level": "P3",
				"bug_type_id": 8,
				"bug_type": "前端-开发代码",
				"status": "active",
				"status_name": "激活",
				"resolve_solution": null,
				"resolve_solution_name": "",
				"creator": {"mate_id": 2076, "name": "李景华"},
				"assignee": {"mate_id": 2401, "name": "刘鹏", "dept_name": "研发中心/技术部/前端组"},
				"module": {"module_id": 91, "module_name": "统计"},
				"attachments": [],
				"videos": [],
				"bug_detail": {
					"bug_id": 1081,
					"title": "【生成报告】iOS 14.6/16.1 白屏",
					"description": "<p>[步骤]</p><p>打开生成报告</p>",
					"bug_level": "P3",
					"priority": "一般",
					"bug_type_id": 8,
					"bug_type_name": "前端-开发代码",
					"status": "active",
					"status_name": "激活",
					"module": {"module_id": 91, "module_name": "统计"},
					"version": {"version_id": 163, "version_name": "v2.91.56-企业一体化项目看板", "version_type": 1, "version_status": 8},
					"creator": {"mate_id": 2076, "name": "李景华"},
					"assignee": {"mate_id": 2401, "name": "刘鹏", "dept_name": "研发中心/技术部/前端组"},
					"bug_url": "https://zentao.lggj.work/zentao/bug-view-29593.html",
					"source_url": "http://192.168.215.31:9001/#/qms/bugCenter/bugManager?bugId=1081",
					"attachments": [],
					"videos": []
				},
				"labels": ["syndra", "frontend", "bug", "P3"],
				"source_url": "http://192.168.215.31:9001/#/qms/bugCenter/bugManager?bugId=1081",
				"metadata": {"syndra_role": "frontend"}
			}]
		}`),
	)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	testHandler.ImportExternalIssue(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Status   string        `json:"status"`
		SyncType string        `json:"sync_type"`
		Existing bool          `json:"existing"`
		Issue    IssueResponse `json:"issue"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "synced" || resp.SyncType != "bug" || resp.Existing {
		t.Fatalf("response status/sync/existing = %#v", resp)
	}
	if resp.Issue.Title != "【Bug#1081】【v2.91.56-企业一体化项目看板】【生成报告】iOS 14.6/16.1 白屏" || resp.Issue.Status != "todo" || resp.Issue.Priority != "medium" {
		t.Fatalf("issue response = title %q status %q priority %q", resp.Issue.Title, resp.Issue.Status, resp.Issue.Priority)
	}
	if resp.Issue.Metadata["bug_level"] != "P3" ||
		resp.Issue.Metadata["bug_type_id"] != float64(8) ||
		resp.Issue.Metadata["bug_creator_name"] != "李景华" ||
		resp.Issue.Metadata["bug_module_name"] != "统计" ||
		resp.Issue.Metadata["bug_version_status"] != float64(8) ||
		resp.Issue.Metadata["bug_zentao_url"] != "https://zentao.lggj.work/zentao/bug-view-29593.html" ||
		resp.Issue.Metadata["syndra_role"] != "frontend" {
		t.Fatalf("metadata = %#v", resp.Issue.Metadata)
	}
	if resp.Issue.Description == nil || !strings.Contains(*resp.Issue.Description, "[结果]\n白屏") {
		t.Fatalf("description = %#v", resp.Issue.Description)
	}
}

func TestLogExternalBugSyncRequestBodyPreservesBodyForDecode(t *testing.T) {
	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	raw := `{"schema_version":"syndra.multica.version_bug.webhook.v1","event_id":"evt-log-raw-body","items":[]}`
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/webhooks/external-issues?sync_type=bug&workspace_id=query-workspace&assignee_user_id=query-assignee",
		strings.NewReader(raw),
	)

	if err := logExternalBugSyncRequestBody(req); err != nil {
		t.Fatalf("logExternalBugSyncRequestBody: %v", err)
	}
	if logs := buf.String(); !strings.Contains(logs, "evt-log-raw-body") || !strings.Contains(logs, "request_body_bytes="+strconv.Itoa(len(raw))) {
		t.Fatalf("log output did not include raw body marker and byte count: %s", logs)
	}

	got, err := decodeExternalBugSyncRequest(req)
	if err != nil {
		t.Fatalf("decodeExternalBugSyncRequest after logging: %v", err)
	}
	if got.WorkspaceID != "query-workspace" || got.AssigneeUserID != "query-assignee" || got.Payload.EventID != "evt-log-raw-body" {
		t.Fatalf("decoded request = %#v", got)
	}
}

func seedExternalIssueImportIdentity(t *testing.T, externalUserID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testHandler.Queries.UpsertUserExternalIdentityByOpenID(ctx, db.UpsertUserExternalIdentityByOpenIDParams{
		UserID:         parseUUID(testUserID),
		Provider:       enterpriseLark.ProviderName,
		TenantKey:      "handler-test",
		ExternalUserID: pgtype.Text{String: externalUserID, Valid: true},
		OpenID:         pgtype.Text{String: externalUserID + "-open", Valid: true},
		UnionID:        pgtype.Text{String: externalUserID + "-union", Valid: true},
		Email:          pgtype.Text{String: handlerTestEmail, Valid: true},
		Name:           pgtype.Text{String: handlerTestName, Valid: true},
		AvatarUrl:      pgtype.Text{},
		RawProfile:     []byte(`{"source":"handler-test"}`),
	}); err != nil {
		t.Fatalf("UpsertUserExternalIdentityByOpenID: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `
			DELETE FROM user_external_identity
			WHERE provider = $1 AND tenant_key = $2 AND external_user_id = $3
		`, enterpriseLark.ProviderName, "handler-test", externalUserID)
	})
}

func TestImportExternalIssueIgnoresNonTargetVersionType(t *testing.T) {
	t.Setenv("MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN", "secret")

	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/webhooks/external-issues", map[string]any{
		"workspace_id": testWorkspaceID,
		"record_id":    "rec-not-target",
		"fields": map[string]any{
			"版本类型": "大需求",
			"版本":   "MUL-ignored",
		},
	})
	req.Header.Set("Authorization", "Bearer secret")
	testHandler.ImportExternalIssue(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	assertJSONEqual(t, w.Body.Bytes(), `{
		"status": "ignored",
		"reason": "version_type_not_target",
		"provider": "lark_base",
		"source_record_id": "rec-not-target"
	}`)
}

func TestImportExternalIssueRequiresLarkAppForRecordLookup(t *testing.T) {
	t.Setenv("MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN", "secret")
	t.Setenv("MULTICA_EXTERNAL_ISSUE_DEFAULT_WORKSPACE_ID", testWorkspaceID)
	t.Setenv("LARK_APP_ID", "")
	t.Setenv("LARK_APP_SECRET", "")

	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/webhooks/external-issues", map[string]any{
		"app_token": "base-token",
		"table_id":  "table-id",
		"record_id": "record-id",
	})
	req.Header.Set("Authorization", "Bearer secret")
	testHandler.ImportExternalIssue(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

func TestImportExternalIssueAcceptsFeishuParamsWithoutRequestBody(t *testing.T) {
	t.Setenv("MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN", "secret")
	t.Setenv("MULTICA_EXTERNAL_ISSUE_DEFAULT_WORKSPACE_ID", testWorkspaceID)
	t.Setenv("LARK_APP_ID", "")
	t.Setenv("LARK_APP_SECRET", "")

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/external-issues?app_token=base-token&table_id=table-id&record_id=record-id", nil)
	req.Header.Set("Authorization", "Bearer secret")
	testHandler.ImportExternalIssue(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 from Lark app config branch; body=%s", w.Code, w.Body.String())
	}
}

func TestExternalIssueImportTimeoutReturnsGatewayTimeout(t *testing.T) {
	w := httptest.NewRecorder()
	writeExternalImportError(w, externalissue.ErrLarkOpenAPITimeout)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s", w.Code, w.Body.String())
	}
}

func TestDecodeExternalIssueImportRequestMergesQueryParams(t *testing.T) {
	req := newRequest(http.MethodPost, "/api/webhooks/external-issues?app_token=query-base&table_id=query-table&record_id=query-record&workspace_id=query-workspace", map[string]any{
		"app_token": "body-base",
		"table_id":  "body-table",
		"record_id": "body-record",
	})

	got, err := decodeExternalIssueImportRequest(req)
	if err != nil {
		t.Fatalf("decodeExternalIssueImportRequest: %v", err)
	}
	if got.AppToken != "query-base" || got.TableID != "query-table" || got.RecordID != "query-record" || got.WorkspaceID != "query-workspace" {
		t.Fatalf("decoded request = %#v", got)
	}
}
