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
	t.Setenv("MULTICA_EXTERNAL_BUG_WORKSPACE_ID", testWorkspaceID)

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
				"assignee": {"mate_id": 2401, "name": "Handler Test User", "dept_name": "研发中心/技术部/前端组"},
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
					"assignee": {"mate_id": 2401, "name": "Handler Test User", "dept_name": "研发中心/技术部/前端组"},
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
	if resp.Issue.AssigneeType == nil || *resp.Issue.AssigneeType != "member" ||
		resp.Issue.AssigneeID == nil || *resp.Issue.AssigneeID != testUserID ||
		resp.Issue.CreatorID != testUserID {
		t.Fatalf("issue assignee/creator = %#v/%#v/%s", resp.Issue.AssigneeType, resp.Issue.AssigneeID, resp.Issue.CreatorID)
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
		"/api/webhooks/external-issues?sync_type=bug&workspace_id=ignored-workspace&assignee_user_id=ignored-assignee",
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
	if got.Payload.EventID != "evt-log-raw-body" {
		t.Fatalf("decoded request = %#v", got)
	}
}

func TestImportExternalIssueRequirementSyncCreatesAndDispatchesIssue(t *testing.T) {
	t.Setenv("MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN", "secret")
	t.Setenv("MULTICA_EXTERNAL_REQUIREMENT_WORKSPACE_ID", testWorkspaceID)
	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	agentID := createHandlerTestAgent(t, "syndra-requirement-sync", []byte(`{}`))
	if _, err := testPool.Exec(context.Background(), `
		UPDATE agent SET model = 'agent-owned-model', thinking_level = 'high'
		WHERE id = $1
	`, agentID); err != nil {
		t.Fatalf("configure agent runtime settings: %v", err)
	}
	sourceRecordID := "wu_requirement-log-marker_" + strings.ReplaceAll(agentID, "-", "")
	payload := map[string]any{
		"id":                   sourceRecordID,
		"title":                "v3.1307_kk协议数据修复",
		"description":          "This field is not used as the issue description.",
		"size":                 "small",
		"owner_id":             "syndra-owner-2401",
		"product_iteration_id": 2262,
		"project_record_url":   "https://wvyeimw605u.feishu.cn/record/handler-test",
		"development_role":     "fullstack",
		"execution_prompt":     "Syndra 生成的完整 qtb-dev-flow-native 执行 Prompt。",
		"model":                "payload-model-must-be-ignored",
		"reasoning_effort":     "xhigh",
		"state":                "claimable",
		"lane_id":              "lane-handler-test",
		"lane_type":            "fullstack",
		"executor_kind":        "multica",
		"executor_id":          agentID,
		"current_attempt": map[string]any{
			"id":              "att_handler_test",
			"provider_run_id": "prun_handler_test",
		},
		"observer_notification_dispatch_token": "observer_dispatch_handler_test",
		"unknown_field":                        map[string]any{"accepted": true},
	}
	rawBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal requirement payload: %v", err)
	}
	raw := string(rawBytes)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/webhooks/external-issues?sync_type=requirement",
		strings.NewReader(raw),
	)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	testHandler.ImportExternalIssue(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Status             string        `json:"status"`
		SyncType           string        `json:"sync_type"`
		Provider           string        `json:"provider"`
		Existing           bool          `json:"existing"`
		SourceRecordID     string        `json:"source_record_id"`
		ProductIterationID int64         `json:"product_iteration_id"`
		ProjectRecordURL   string        `json:"project_record_url"`
		Issue              IssueResponse `json:"issue"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "synced" || resp.SyncType != "requirement" || resp.Provider != "syndra" || resp.Existing {
		t.Fatalf("response = %#v", resp)
	}
	if resp.SourceRecordID != sourceRecordID || resp.ProductIterationID != 2262 || resp.ProjectRecordURL != payload["project_record_url"] {
		t.Fatalf("response source mapping = %#v", resp)
	}
	dispatchKey := "syndra-flow-v1:" + sourceRecordID + ":att_handler_test"
	wantTitle := "[SYN:v1:" + dispatchKey + "] v3.1307_kk协议数据修复"
	wantDescription := "结构化需求源：\n" +
		"external.product_iteration_id=2262\n" +
		"project_record_url=https://wvyeimw605u.feishu.cn/record/handler-test\n" +
		"development_role=fullstack\n\n" +
		"Syndra 生成的完整 qtb-dev-flow-native 执行 Prompt。\n\n" +
		"---\n\n" +
		"[SYN:v1:" + dispatchKey + "]\n" +
		"dispatch_key=" + dispatchKey + "\n" +
		"lane_id=lane-handler-test\n" +
		"work_unit_id=" + sourceRecordID + "\n" +
		"external.lane_type=fullstack\n" +
		"external.provider_run_id=prun_handler_test"
	if resp.Issue.Title != wantTitle || resp.Issue.Description == nil || *resp.Issue.Description != wantDescription {
		t.Fatalf("issue title/description = %q / %#v", resp.Issue.Title, resp.Issue.Description)
	}
	if resp.Issue.Status != "todo" || resp.Issue.Priority != "none" || resp.Issue.AssigneeType == nil || *resp.Issue.AssigneeType != "agent" || resp.Issue.AssigneeID == nil || *resp.Issue.AssigneeID != agentID {
		t.Fatalf("issue dispatch fields = status=%q priority=%q assignee_type=%#v assignee_id=%#v", resp.Issue.Status, resp.Issue.Priority, resp.Issue.AssigneeType, resp.Issue.AssigneeID)
	}
	if resp.Issue.CreatorType != "member" || resp.Issue.CreatorID != testUserID {
		t.Fatalf("issue creator = %s/%s, want member/%s", resp.Issue.CreatorType, resp.Issue.CreatorID, testUserID)
	}
	if resp.Issue.Metadata["source_record_id"] != sourceRecordID ||
		resp.Issue.Metadata["syndra_requirement_id"] != float64(2262) ||
		resp.Issue.Metadata["project_record_url"] != payload["project_record_url"] ||
		resp.Issue.Metadata["syndra_owner_id"] != payload["owner_id"] ||
		resp.Issue.Metadata["syndra_executor_id"] != agentID ||
		resp.Issue.Metadata["dispatch_key"] != dispatchKey ||
		resp.Issue.Metadata["attempt_id"] != "att_handler_test" ||
		resp.Issue.Metadata["provider_run_id"] != "prun_handler_test" {
		t.Fatalf("issue metadata = %#v", resp.Issue.Metadata)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, resp.Issue.ID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, resp.Issue.ID)
	})
	tasks, err := testHandler.Queries.ListTasksByIssue(context.Background(), parseUUID(resp.Issue.ID))
	if err != nil {
		t.Fatalf("ListTasksByIssue: %v", err)
	}
	if len(tasks) != 1 || tasks[0].AgentID != parseUUID(agentID) || tasks[0].Status != "queued" {
		t.Fatalf("automatic development tasks = %#v", tasks)
	}
	agent, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(agentID))
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if !agent.Model.Valid || agent.Model.String != "agent-owned-model" || !agent.ThinkingLevel.Valid || agent.ThinkingLevel.String != "high" {
		t.Fatalf("agent runtime settings were overwritten: model=%#v thinking_level=%#v", agent.Model, agent.ThinkingLevel)
	}
	if logs := buf.String(); !strings.Contains(logs, "requirement-log-marker") ||
		!strings.Contains(logs, "request_body_bytes="+strconv.Itoa(len(raw))) ||
		!strings.Contains(logs, "external requirement sync: executor resolved") ||
		!strings.Contains(logs, "external requirement sync: automatic development task confirmed") ||
		!strings.Contains(logs, "model_field_ignored=true") ||
		!strings.Contains(logs, "reasoning_effort_field_ignored=true") {
		t.Fatalf("log output did not include raw body marker and byte count: %s", logs)
	}

	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/webhooks/external-issues?sync_type=requirement", strings.NewReader(raw))
	req2.Header.Set("Authorization", "Bearer secret")
	req2.Header.Set("Content-Type", "application/json")
	testHandler.ImportExternalIssue(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("idempotent status = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	var repeated struct {
		Existing bool          `json:"existing"`
		Issue    IssueResponse `json:"issue"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &repeated); err != nil {
		t.Fatalf("decode idempotent response: %v", err)
	}
	if !repeated.Existing || repeated.Issue.ID != resp.Issue.ID {
		t.Fatalf("idempotent response = %#v, original issue = %s", repeated, resp.Issue.ID)
	}
	tasks, err = testHandler.Queries.ListTasksByIssue(context.Background(), parseUUID(resp.Issue.ID))
	if err != nil {
		t.Fatalf("ListTasksByIssue after idempotent request: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("idempotent request created %d tasks, want 1", len(tasks))
	}
}

func TestImportExternalIssueRejectsRemovedFeishuImport(t *testing.T) {
	t.Setenv("MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN", "secret")

	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/webhooks/external-issues", map[string]any{
		"app_token": "base-token",
		"table_id":  "table-id",
		"record_id": "record-id",
	})
	req.Header.Set("Authorization", "Bearer secret")
	testHandler.ImportExternalIssue(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unsupported external issue sync_type") {
		t.Fatalf("body = %s, want unsupported sync_type error", w.Body.String())
	}
}
