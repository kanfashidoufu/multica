package externalissue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/analytics"
	enterpriseLark "github.com/multica-ai/multica/server/internal/enterprise/lark"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type memoryStorage struct {
	mu    sync.Mutex
	files map[string][]byte
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func (s *memoryStorage) Upload(_ context.Context, key string, data []byte, _ string, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[key] = append([]byte(nil), data...)
	return "https://cdn.example.test/" + key, nil
}

func (s *memoryStorage) Delete(_ context.Context, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, key)
}

func (s *memoryStorage) DeleteKeys(ctx context.Context, keys []string) {
	for _, key := range keys {
		s.Delete(ctx, key)
	}
}

func (s *memoryStorage) KeyFromURL(rawURL string) string {
	return strings.TrimPrefix(rawURL, "https://cdn.example.test/")
}

func (s *memoryStorage) CdnDomain() string { return "cdn.example.test" }

func (s *memoryStorage) GetReader(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return io.NopCloser(strings.NewReader(string(s.files[key]))), nil
}

func TestImportCreatesBacklogIssueAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	storage := &memoryStorage{}

	attachmentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Disposition", `attachment; filename="spec.txt"`)
		_, _ = w.Write([]byte("attachment body"))
	}))
	t.Cleanup(attachmentServer.Close)

	serviceBus := events.New()
	importer := &Importer{
		Queries: q,
		IssueService: service.NewIssueService(
			q,
			pool,
			serviceBus,
			analytics.NoopClient{},
			nil,
		),
		Storage: storage,
		Bus:     events.New(),
		BroadcastPayload: func(_ context.Context, issue db.Issue, attachments []db.Attachment) map[string]any {
			return map[string]any{
				"issue": map[string]any{
					"id":               util.UUIDToString(issue.ID),
					"attachment_count": len(attachments),
				},
			}
		},
		Config: Config{
			WebhookToken:                  "test-token",
			DefaultAssigneeExternalUserID: fx.ExternalUserID,
		},
	}
	var sawCreateEvent bool
	serviceBus.Subscribe("issue:created", func(e events.Event) {
		payload, _ := e.Payload.(map[string]any)
		issue, _ := payload["issue"].(map[string]any)
		sawCreateEvent = issue["id"] != "" && issue["attachment_count"] == 1
	})
	var sawInboxEvent bool
	importer.Bus.Subscribe("inbox:new", func(e events.Event) {
		payload, _ := e.Payload.(map[string]any)
		item, _ := payload["item"].(map[string]any)
		sawInboxEvent = item["type"] == "issue_assigned" &&
			item["recipient_id"] == util.UUIDToString(fx.User.ID)
	})

	req := Request{
		Provider:    "lark_base",
		WorkspaceID: util.UUIDToString(fx.Workspace.ID),
		BaseToken:   "base-token",
		TableID:     "table-id",
		RecordID:    "record-1",
		RecordURL:   "https://example.feishu.cn/base/base-token?table=table-id",
		Fields: map[string]any{
			"版本类型": "小需求",
			"版本":   "MUL-100",
			"需求名称": "同步飞书需求",
			"备注":   "第一版备注",
			"附件": []any{
				map[string]any{
					"name": "spec.txt",
					"url":  attachmentServer.URL + "/spec.txt",
				},
			},
		},
	}

	first, err := importer.Import(ctx, req)
	if err != nil {
		t.Fatalf("Import first: %v", err)
	}
	if first.Existing {
		t.Fatalf("first import unexpectedly marked existing")
	}
	if first.Issue.Title != "MUL-100" {
		t.Fatalf("title = %q", first.Issue.Title)
	}
	if first.Issue.Status != "backlog" {
		t.Fatalf("status = %q, want backlog", first.Issue.Status)
	}
	if first.Issue.Priority != "none" {
		t.Fatalf("priority = %q, want none", first.Issue.Priority)
	}
	if first.Issue.AssigneeType.String != "member" || first.Issue.AssigneeID != fx.User.ID {
		t.Fatalf("assignee = (%q, %s), want member %s", first.Issue.AssigneeType.String, util.UUIDToString(first.Issue.AssigneeID), util.UUIDToString(fx.User.ID))
	}
	if !strings.Contains(first.Issue.Description.String, "需求名称：同步飞书需求") ||
		!strings.Contains(first.Issue.Description.String, "备注：第一版备注") ||
		!strings.Contains(first.Issue.Description.String, "来源：https://example.feishu.cn/base/base-token?table=table-id") {
		t.Fatalf("description did not include merged fields and source: %q", first.Issue.Description.String)
	}
	if len(first.Attachments) != 1 {
		t.Fatalf("attachments len = %d, want 1; errors=%v", len(first.Attachments), first.AttachmentErrors)
	}
	if first.Attachments[0].Filename != "spec.txt" {
		t.Fatalf("attachment filename = %q", first.Attachments[0].Filename)
	}
	if want := fmt.Sprintf("!file[spec.txt](%s)", first.Attachments[0].Url); !strings.Contains(first.Issue.Description.String, want) {
		t.Fatalf("description missing file-card markdown %q: %q", want, first.Issue.Description.String)
	}
	assertAssigneeSubscribedAndInbox(t, ctx, q, pool, first.Issue.ID, fx.User.ID)
	if !sawInboxEvent {
		t.Fatalf("external import did not publish issue_assigned inbox event")
	}
	if !sawCreateEvent {
		t.Fatalf("external import did not publish full issue:created payload")
	}

	second, err := importer.Import(ctx, req)
	if err != nil {
		t.Fatalf("Import second: %v", err)
	}
	if !second.Existing {
		t.Fatalf("second import did not report existing")
	}
	if second.Issue.ID != first.Issue.ID {
		t.Fatalf("second issue id = %s, want %s", util.UUIDToString(second.Issue.ID), util.UUIDToString(first.Issue.ID))
	}
	assertAssigneeInboxCount(t, ctx, pool, first.Issue.ID, fx.User.ID, 1)
}

func TestImportBugSyncCreatesAndUpdatesIssue(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)

	bus := events.New()
	importer := &Importer{
		Queries: q,
		IssueService: service.NewIssueService(
			q,
			pool,
			bus,
			analytics.NoopClient{},
			nil,
		),
		Bus: bus,
		Config: Config{
			WebhookToken:                  "test-token",
			DefaultWorkspaceID:            util.UUIDToString(fx.Workspace.ID),
			DefaultAssigneeExternalUserID: fx.ExternalUserID,
		},
	}

	var metadataEvents int
	bus.Subscribe("issue_metadata:changed", func(e events.Event) {
		metadataEvents++
	})
	payload := BugSyncPayload{
		SchemaVersion: "syndra.multica.version_bug.webhook.v1",
		EventType:     "version_bug.changed",
		EventID:       "event-1081",
		Scene:         "frontend_debug",
		Source:        "syndra",
		SourceEnv:     "local",
		SentAt:        "2026-06-30T10:50:25+08:00",
		Items: []BugSyncItem{{
			Event:       "upsert",
			EntityType:  "version_bug",
			ExternalKey: "syndra:local:version_bug:1081",
			BugID:       1081,
			VersionID:   163,
			VersionName: "v2.91.56-企业一体化项目看板",
			DemandName:  "-企业一体化项目看板",
			Role:        "frontend",
			Title:       "【生成报告】iOS 14.6 白屏",
			Description: "版本：v2.91.56\n严重程度：P3\n<p>[步骤]</p><p>打开生成报告后白屏</p><br><p>[期望]</p><p>正常展示</p>",
			Priority:    "一般",
			BugLevel:    "P3",
			BugTypeID:   8,
			BugType:     "前端-开发代码",
			Status:      "active",
			StatusName:  "激活",
			Creator:     BugSyncPerson{MateID: int64Ptr(2076), Name: strPtr("李景华")},
			Assignee:    BugSyncPerson{MateID: int64Ptr(2401), Name: strPtr("刘鹏"), DeptName: strPtr("研发中心/技术部/前端组")},
			Module:      BugSyncModule{ModuleID: 91, ModuleName: "统计"},
			BugDetail: BugSyncDetail{
				BugURL:    "https://zentao.lggj.work/zentao/bug-view-29593.html",
				SourceURL: "http://192.168.215.31:9001/#/qms/bugCenter/bugManager?bugId=1081",
				Version:   BugSyncVersion{VersionID: 163, VersionName: "v2.91.56-企业一体化项目看板", VersionType: 1, VersionStatus: 8},
			},
			Labels:    []string{"syndra", "frontend", "bug", "P3"},
			SourceURL: "http://192.168.215.31:9001/#/qms/bugCenter/bugManager?bugId=1081",
			Metadata: map[string]any{
				"syndra_version_id": "163",
				"syndra_role":       "frontend",
			},
		}},
	}

	first, err := importer.ImportBugSync(ctx, BugSyncRequest{Payload: payload})
	if err != nil {
		t.Fatalf("ImportBugSync first: %v", err)
	}
	if len(first.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(first.Items))
	}
	created := first.Items[0]
	if created.Existing {
		t.Fatalf("first import unexpectedly marked existing")
	}
	if created.Issue.Title != "【Bug#1081】【v2.91.56-企业一体化项目看板】【生成报告】iOS 14.6 白屏" {
		t.Fatalf("title = %q", created.Issue.Title)
	}
	if created.Issue.Status != "todo" {
		t.Fatalf("status = %q, want todo", created.Issue.Status)
	}
	if created.Issue.Priority != "medium" {
		t.Fatalf("priority = %q, want medium", created.Issue.Priority)
	}
	if !strings.Contains(created.Issue.Description.String, "[步骤]\n打开生成报告后白屏") ||
		!strings.Contains(created.Issue.Description.String, "[期望]\n正常展示") {
		t.Fatalf("description was not converted from HTML: %q", created.Issue.Description.String)
	}
	var metadata map[string]any
	if err := json.Unmarshal(created.Issue.Metadata, &metadata); err != nil {
		t.Fatalf("metadata decode: %v", err)
	}
	if metadata["bug_id"] != float64(1081) ||
		metadata["bug_level"] != "P3" ||
		metadata["bug_type_id"] != float64(8) ||
		metadata["bug_creator_name"] != "李景华" ||
		metadata["bug_module_name"] != "统计" ||
		metadata["bug_version_status"] != float64(8) ||
		metadata["bug_zentao_url"] != "https://zentao.lggj.work/zentao/bug-view-29593.html" ||
		metadata["syndra_role"] != "frontend" {
		t.Fatalf("metadata = %#v", metadata)
	}
	assertAssigneeSubscribedAndInbox(t, ctx, q, pool, created.Issue.ID, fx.User.ID)

	payload.Items[0].Title = "【生成报告】iOS 16.1 白屏已定位"
	payload.Items[0].Status = "resolved"
	payload.Items[0].StatusName = "已解决"
	payload.Items[0].BugLevel = "P2"
	second, err := importer.ImportBugSync(ctx, BugSyncRequest{Payload: payload})
	if err != nil {
		t.Fatalf("ImportBugSync second: %v", err)
	}
	if len(second.Items) != 1 || !second.Items[0].Existing {
		t.Fatalf("second result = %#v, want existing item", second.Items)
	}
	updated := second.Items[0].Issue
	if updated.ID != created.Issue.ID {
		t.Fatalf("updated issue id = %s, want %s", util.UUIDToString(updated.ID), util.UUIDToString(created.Issue.ID))
	}
	if updated.Title != "【Bug#1081】【v2.91.56-企业一体化项目看板】【生成报告】iOS 16.1 白屏已定位" || updated.Status != "done" || updated.Priority != "high" {
		t.Fatalf("updated issue = title %q status %q priority %q", updated.Title, updated.Status, updated.Priority)
	}
	if metadataEvents < 2 {
		t.Fatalf("metadata event count = %d, want at least 2", metadataEvents)
	}
	assertAssigneeInboxCount(t, ctx, pool, created.Issue.ID, fx.User.ID, 1)
}

func TestImportBugSyncAssignsMappedBugAssignee(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	bugAssignee := createImporterWorkspaceMember(t, ctx, pool, q, fx.Workspace.ID, "刘鹏", "")

	bus := events.New()
	importer := &Importer{
		Queries: q,
		IssueService: service.NewIssueService(
			q,
			pool,
			bus,
			analytics.NoopClient{},
			nil,
		),
		Bus: bus,
		Config: Config{
			WebhookToken:                  "test-token",
			DefaultWorkspaceID:            util.UUIDToString(fx.Workspace.ID),
			DefaultAssigneeExternalUserID: fx.ExternalUserID,
		},
	}

	res, err := importer.ImportBugSync(ctx, BugSyncRequest{
		Payload: BugSyncPayload{
			Source:    "syndra",
			SourceEnv: "local",
			Items: []BugSyncItem{{
				Event:       "upsert",
				EntityType:  "version_bug",
				ExternalKey: "syndra:local:version_bug:mapped-assignee",
				BugID:       1081,
				Title:       "按 Bug 指派人分配",
				BugLevel:    "P3",
				Status:      "active",
				Assignee:    BugSyncPerson{MateID: int64Ptr(2401), Name: strPtr("刘鹏")},
			}},
		},
	})
	if err != nil {
		t.Fatalf("ImportBugSync: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(res.Items))
	}
	issue := res.Items[0].Issue
	if issue.AssigneeType.String != "member" || issue.AssigneeID != bugAssignee.User.ID {
		t.Fatalf("assignee = (%q, %s), want member %s", issue.AssigneeType.String, util.UUIDToString(issue.AssigneeID), util.UUIDToString(bugAssignee.User.ID))
	}
	if issue.CreatorID != bugAssignee.User.ID {
		t.Fatalf("creator = %s, want matched assignee %s", util.UUIDToString(issue.CreatorID), util.UUIDToString(bugAssignee.User.ID))
	}
	assertAssigneeSubscribedAndInbox(t, ctx, q, pool, issue.ID, bugAssignee.User.ID)
	assertAssigneeInboxCount(t, ctx, pool, issue.ID, fx.User.ID, 0)
}

func TestImportFetchesLarkRecordWhenAutomationSendsOnlyRecordIDs(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	storage := &memoryStorage{}

	var larkServer *httptest.Server
	larkServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			if r.Method != http.MethodPost {
				t.Errorf("token method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"tenant-token","expire":7200}`))
		case "/open-apis/bitable/v1/apps/base-token/tables/table-id/records/record-3":
			if got := r.Header.Get("Authorization"); got != "Bearer tenant-token" {
				t.Errorf("record authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"code": 0,
				"data": {
					"record": {
						"record_id": "record-3",
						"fields": {
							"版本类型": "小需求",
							"版本": "MUL-103",
							"需求名称": "只传三要素",
							"备注": "服务端回查记录",
							"附件": [{
								"file_token": "file-token-1",
								"name": "design.txt",
								"size": 11,
								"type": "txt",
								"url": "` + larkServer.URL + `/open-apis/drive/v1/medias/file-token-1/download"
							}]
						}
					}
				}
			}`))
		case "/open-apis/drive/v1/medias/file-token-1/download":
			if got := r.Header.Get("Authorization"); got != "Bearer tenant-token" {
				t.Errorf("download authorization = %q", got)
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Disposition", `attachment; filename="design.txt"`)
			_, _ = w.Write([]byte("hello world"))
		default:
			t.Fatalf("unexpected lark request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(larkServer.Close)

	importer := &Importer{
		Queries: q,
		IssueService: service.NewIssueService(
			q,
			pool,
			events.New(),
			analytics.NoopClient{},
			nil,
		),
		Storage:    storage,
		HTTPClient: larkServer.Client(),
		Config: Config{
			WebhookToken:                  "test-token",
			DefaultWorkspaceID:            util.UUIDToString(fx.Workspace.ID),
			DefaultAssigneeExternalUserID: fx.ExternalUserID,
			LarkAppID:                     "cli_test",
			LarkAppSecret:                 "secret",
			LarkOpenAPIBaseURL:            larkServer.URL,
		},
	}

	res, err := importer.Import(ctx, Request{
		AppToken: "base-token",
		TableID:  "table-id",
		RecordID: "record-3",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Issue.Title != "MUL-103" {
		t.Fatalf("title = %q", res.Issue.Title)
	}
	if res.Issue.WorkspaceID != fx.Workspace.ID {
		t.Fatalf("workspace id = %s", util.UUIDToString(res.Issue.WorkspaceID))
	}
	if !strings.Contains(res.Issue.Description.String, "需求名称：只传三要素") ||
		!strings.Contains(res.Issue.Description.String, "备注：服务端回查记录") {
		t.Fatalf("description = %q", res.Issue.Description.String)
	}
	if len(res.Attachments) != 1 {
		t.Fatalf("attachments len = %d, errors=%v", len(res.Attachments), res.AttachmentErrors)
	}
	if res.Attachments[0].Filename != "design.txt" {
		t.Fatalf("attachment filename = %q", res.Attachments[0].Filename)
	}
}

func TestImportAppendsImageAttachmentMarkdownToDescription(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	storage := &memoryStorage{}

	attachmentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", `attachment; filename="screen[1](draft).png"`)
		_, _ = w.Write([]byte("png body"))
	}))
	t.Cleanup(attachmentServer.Close)

	importer := &Importer{
		Queries: q,
		IssueService: service.NewIssueService(
			q,
			pool,
			events.New(),
			analytics.NoopClient{},
			nil,
		),
		Storage: storage,
		Config: Config{
			WebhookToken:                  "test-token",
			DefaultAssigneeExternalUserID: fx.ExternalUserID,
		},
	}

	res, err := importer.Import(ctx, Request{
		Provider:    "lark_base",
		WorkspaceID: util.UUIDToString(fx.Workspace.ID),
		BaseToken:   "base-token",
		TableID:     "table-id",
		RecordID:    "record-image",
		Fields: map[string]any{
			"版本类型": "小需求",
			"版本":   "MUL-104",
			"需求名称": "同步图片附件",
			"附件": []any{
				map[string]any{
					"name": "screen[1](draft).png",
					"url":  attachmentServer.URL + "/screen.png",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.Attachments) != 1 {
		t.Fatalf("attachments len = %d, errors=%v", len(res.Attachments), res.AttachmentErrors)
	}
	want := fmt.Sprintf("![screen\\[1\\]\\(draft\\).png](%s)", res.Attachments[0].Url)
	if !strings.Contains(res.Issue.Description.String, want) {
		t.Fatalf("description missing image markdown %q: %q", want, res.Issue.Description.String)
	}
}

func TestImportIgnoresNonTargetVersionTypeBeforeAssigneeLookup(t *testing.T) {
	importer := &Importer{
		Queries:      &db.Queries{},
		IssueService: &service.IssueService{},
		Config: Config{
			WebhookToken: "test-token",
		},
	}

	res, err := importer.Import(context.Background(), Request{
		WorkspaceID: "11111111-1111-1111-1111-111111111111",
		RecordID:    "record-2",
		Fields: map[string]any{
			"版本类型": "大需求",
			"版本":   "MUL-101",
		},
	})
	if err != nil {
		t.Fatalf("Import returned error for ignored record: %v", err)
	}
	if !res.Ignored || res.Reason != "version_type_not_target" {
		t.Fatalf("ignored result = %#v", res)
	}
}

func TestDecodeRequestSupportsRecordEnvelopeAndNumericFields(t *testing.T) {
	req, err := DecodeRequest(strings.NewReader(`{
		"workspace_id": "11111111-1111-1111-1111-111111111111",
		"record": {
			"record_id": "rec123",
			"fields": {
				"版本类型": "小需求",
				"版本": 20260609,
				"需求名称": [{"text": "名称"}],
				"附件": [{"file_token": "token-a", "name": "a.txt"}]
			}
		}
	}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	rec, err := (&Importer{}).normalize(req)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec.RecordID != "rec123" {
		t.Fatalf("record id = %q", rec.RecordID)
	}
	if rec.Title != "20260609" {
		t.Fatalf("title = %q", rec.Title)
	}
	if rec.Name != "名称" {
		t.Fatalf("name = %q", rec.Name)
	}
	if len(rec.Attachments) != 1 || rec.Attachments[0].FileToken != "token-a" {
		t.Fatalf("attachments = %#v", rec.Attachments)
	}
}

func TestDecodeBugSyncRequestUnwrapsDebugPushResponse(t *testing.T) {
	payload, err := DecodeBugSyncRequest(strings.NewReader(`{
		"code": 4000,
		"success": true,
		"data": {
			"dry_run": true,
			"payload": {
				"schema_version": "syndra.multica.version_bug.webhook.v1",
				"source": "syndra",
				"items": [{
					"event": "upsert",
					"entity_type": "version_bug",
					"external_key": "syndra:local:version_bug:1081",
					"bug_id": 1081,
					"title": "白屏"
				}]
			}
		}
	}`))
	if err != nil {
		t.Fatalf("DecodeBugSyncRequest: %v", err)
	}
	if payload.SchemaVersion != "syndra.multica.version_bug.webhook.v1" || len(payload.Items) != 1 {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Items[0].ExternalKey != "syndra:local:version_bug:1081" {
		t.Fatalf("external key = %q", payload.Items[0].ExternalKey)
	}
}

func TestDecodeRequestUsesRecordIDWithTopLevelFields(t *testing.T) {
	req, err := DecodeRequest(strings.NewReader(`{
		"workspace_id": "11111111-1111-1111-1111-111111111111",
		"record": {"record_id": "rec-top"},
		"fields": {
			"版本类型": "小需求",
			"版本": "MUL-102"
		}
	}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	rec, err := (&Importer{}).normalize(req)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if rec.RecordID != "rec-top" {
		t.Fatalf("record id = %q", rec.RecordID)
	}
	if rec.Title != "MUL-102" {
		t.Fatalf("title = %q", rec.Title)
	}
}

func TestVerifyToken(t *testing.T) {
	importer := &Importer{Config: Config{WebhookToken: "secret"}}
	if err := importer.VerifyToken("Bearer secret"); err != nil {
		t.Fatalf("VerifyToken bearer: %v", err)
	}
	if err := importer.VerifyToken("secret"); err != nil {
		t.Fatalf("VerifyToken raw: %v", err)
	}
	if err := importer.VerifyToken("Bearer wrong"); err != ErrUnauthorized {
		t.Fatalf("wrong token error = %v", err)
	}
}

func TestLarkOpenAPITimeoutIsClassified(t *testing.T) {
	importer := &Importer{
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, context.DeadlineExceeded
			}),
		},
		Config: Config{
			LarkOpenAPIBaseURL: "https://open.feishu.example.test",
			LarkOpenAPITimeout: time.Second,
		},
	}

	err := importer.doLarkAppJSON(context.Background(), http.MethodGet, "/open-apis/bitable/v1/apps/app/tables/table/records/record", "", nil, nil)
	if !errors.Is(err, ErrLarkOpenAPITimeout) {
		t.Fatalf("error = %v, want ErrLarkOpenAPITimeout", err)
	}
}

func TestAttachmentSizePrefersDeclaredValue(t *testing.T) {
	if got := attachmentSize(42, 7); got != 42 {
		t.Fatalf("attachmentSize declared = %d, want 42", got)
	}
	if got := attachmentSize(0, 7); got != 7 {
		t.Fatalf("attachmentSize fallback = %d, want 7", got)
	}
}

func TestAttachmentSourcesSupportsLarkAttachmentFieldShape(t *testing.T) {
	sources := attachmentSources([]any{
		map[string]any{
			"file_token": "file-token",
			"name":       "spec.txt",
			"size":       float64(123),
			"type":       "txt",
			"url":        "https://open.feishu.cn/open-apis/drive/v1/medias/file-token/download",
			"tmp_url":    "https://tmp.example.test/spec.txt",
		},
		map[string]any{
			"file_token": "image-token",
			"name":       "image.png",
			"type":       "image/png",
		},
	})
	if len(sources) != 2 {
		t.Fatalf("sources len = %d", len(sources))
	}
	if sources[0].FileToken != "file-token" || sources[0].Name != "spec.txt" || sources[0].SizeBytes != 123 {
		t.Fatalf("source[0] = %#v", sources[0])
	}
	if sources[0].ContentType != "" {
		t.Fatalf("source[0] content type = %q, want empty for extension-like type", sources[0].ContentType)
	}
	if sources[0].TmpURL != "https://tmp.example.test/spec.txt" {
		t.Fatalf("source[0] tmp url = %q", sources[0].TmpURL)
	}
	if sources[1].ContentType != "image/png" {
		t.Fatalf("source[1] content type = %q", sources[1].ContentType)
	}
}

func assertAssigneeSubscribedAndInbox(t *testing.T, ctx context.Context, q *db.Queries, pool *pgxpool.Pool, issueID, userID pgtype.UUID) {
	t.Helper()
	subscribed, err := q.IsIssueSubscriber(ctx, db.IsIssueSubscriberParams{
		IssueID:  issueID,
		UserType: "member",
		UserID:   userID,
	})
	if err != nil {
		t.Fatalf("IsIssueSubscriber: %v", err)
	}
	if !subscribed {
		t.Fatalf("assignee was not subscribed to external issue")
	}
	assertAssigneeInboxCount(t, ctx, pool, issueID, userID, 1)
}

func assertAssigneeInboxCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID, userID pgtype.UUID, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM inbox_item
		WHERE issue_id = $1
		  AND recipient_type = 'member'
		  AND recipient_id = $2
		  AND type = 'issue_assigned'
	`, issueID, userID).Scan(&n); err != nil {
		t.Fatalf("count issue_assigned inbox: %v", err)
	}
	if n != want {
		t.Fatalf("issue_assigned inbox count = %d, want %d", n, want)
	}
}

func strPtr(v string) *string {
	return &v
}

func int64Ptr(v int64) *int64 {
	return &v
}

type importerFixture struct {
	Workspace      db.Workspace
	User           db.User
	ExternalUserID string
}

type importerMemberFixture struct {
	User           db.User
	Member         db.Member
	ExternalUserID string
}

func openTestPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("could not connect to database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database not reachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func createImporterFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q *db.Queries) importerFixture {
	t.Helper()
	suffix := time.Now().UnixNano()
	user, err := q.CreateUser(ctx, db.CreateUserParams{
		Name:      "External Issue Import Test",
		Email:     fmt.Sprintf("external-issue-import-%d@example.test", suffix),
		AvatarUrl: pgtype.Text{},
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, user.ID)
	})

	workspace, err := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{
		Name:        "External Issue Import Test",
		Slug:        fmt.Sprintf("external-issue-import-%d", suffix),
		Description: pgtype.Text{},
		Context:     pgtype.Text{},
		IssuePrefix: "EIT",
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	t.Cleanup(func() {
		_ = q.DeleteWorkspace(context.Background(), workspace.ID)
	})

	if _, err := q.CreateMember(ctx, db.CreateMemberParams{
		WorkspaceID: workspace.ID,
		UserID:      user.ID,
		Role:        "owner",
	}); err != nil {
		t.Fatalf("CreateMember: %v", err)
	}

	externalUserID := fmt.Sprintf("ou_%d", suffix)
	if _, err := q.UpsertUserExternalIdentityByOpenID(ctx, db.UpsertUserExternalIdentityByOpenIDParams{
		UserID:         user.ID,
		Provider:       enterpriseLark.ProviderName,
		TenantKey:      "tenant-test",
		ExternalUserID: pgtype.Text{String: externalUserID, Valid: true},
		OpenID:         pgtype.Text{String: fmt.Sprintf("open_%d", suffix), Valid: true},
		UnionID:        pgtype.Text{String: fmt.Sprintf("union_%d", suffix), Valid: true},
		Email:          pgtype.Text{String: user.Email, Valid: true},
		Name:           pgtype.Text{String: user.Name, Valid: true},
		AvatarUrl:      pgtype.Text{},
		RawProfile:     []byte(`{"source":"test"}`),
	}); err != nil {
		t.Fatalf("UpsertUserExternalIdentityByOpenID: %v", err)
	}

	return importerFixture{
		Workspace:      workspace,
		User:           user,
		ExternalUserID: externalUserID,
	}
}

func createImporterWorkspaceMember(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q *db.Queries, workspaceID pgtype.UUID, name, externalUserID string) importerMemberFixture {
	t.Helper()
	suffix := time.Now().UnixNano()
	user, err := q.CreateUser(ctx, db.CreateUserParams{
		Name:      name,
		Email:     fmt.Sprintf("external-issue-import-member-%d@example.test", suffix),
		AvatarUrl: pgtype.Text{},
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, user.ID)
	})

	member, err := q.CreateMember(ctx, db.CreateMemberParams{
		WorkspaceID: workspaceID,
		UserID:      user.ID,
		Role:        "member",
	})
	if err != nil {
		t.Fatalf("CreateMember: %v", err)
	}

	if externalUserID != "" {
		if _, err := q.UpsertUserExternalIdentityByOpenID(ctx, db.UpsertUserExternalIdentityByOpenIDParams{
			UserID:         user.ID,
			Provider:       enterpriseLark.ProviderName,
			TenantKey:      "tenant-test",
			ExternalUserID: pgtype.Text{String: externalUserID, Valid: true},
			OpenID:         pgtype.Text{String: fmt.Sprintf("open_%s_%d", externalUserID, suffix), Valid: true},
			UnionID:        pgtype.Text{String: fmt.Sprintf("union_%s_%d", externalUserID, suffix), Valid: true},
			Email:          pgtype.Text{String: user.Email, Valid: true},
			Name:           pgtype.Text{String: user.Name, Valid: true},
			AvatarUrl:      pgtype.Text{},
			RawProfile:     []byte(`{"source":"test"}`),
		}); err != nil {
			t.Fatalf("UpsertUserExternalIdentityByOpenID: %v", err)
		}
	}

	return importerMemberFixture{
		User:           user,
		Member:         member,
		ExternalUserID: externalUserID,
	}
}
