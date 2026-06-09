package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
