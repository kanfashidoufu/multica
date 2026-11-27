package handler

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/externalissue"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/notifications"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type recordingBugBot struct {
	lark.APIClient
	sent []lark.SendTextParams
}

func (b *recordingBugBot) SendTextMessage(_ context.Context, p lark.SendTextParams) (string, error) {
	b.sent = append(b.sent, p)
	return "om_fixture", nil
}

func TestSyndraExpertOriginalBugAndFeishu(t *testing.T) {
	t.Setenv("MULTICA_EXTERNAL_BUG_AUTOMATION_ASSIGNEES", handlerTestName)
	ctx := t.Context()
	ws := dbfx.Workspace(t, "Fixture", externalissue.ExpertWorkspace, testutil.Cols{"issue_prefix": "QTB"})
	f := testutil.New(testPool, ws, testUserID)
	f.Member(t, ws, testUserID, "owner")
	other := f.User(t, "Other", uuid.NewString()+"@example.test")
	f.Member(t, ws, other, "member")
	rt := f.Runtime(t, "Syndra test runtime")
	expert := f.Agent(t, externalissue.BugExpertName, rt, testutil.Cols{"visibility": "workspace", "permission_mode": "public_to"})
	f.Insert(t, "agent_invocation_target", testutil.Cols{"agent_id": expert, "target_type": "workspace", "target_id": ws})
	for _, table := range []string{"issue", "inbox_item", "issue_wakeup", "comment", "activity_log", "channel_installation", "channel_user_binding"} {
		f.Cleanup(t, "DELETE FROM "+table+" WHERE workspace_id=$1", ws)
	}
	f.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE workspace_id=$1)", ws)
	f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE workspace_id=$1)", ws)
	meta, _ := json.Marshal(map[string]any{"external_source": "syndra", "external_key": "syndra:prod:version_bug:42", "bug_entity_type": "version_bug", "sync_source_env": "prod", "bug_assignee_name": handlerTestName, "bug_version_id": 2524, "multica_bug_automation": true})
	bug := f.Issue(t, "Imported bug", testutil.Cols{"number": 1974, "origin_type": "external_issue", "origin_id": "dd3637be-9385-447b-86d1-9547ddb1feda", "metadata": meta, "assignee_type": "agent", "assignee_id": expert})
	f.Exec(t, "UPDATE workspace SET issue_counter=1974 WHERE id=$1", ws)
	task := f.Task(t, expert, testutil.Cols{"runtime_id": rt, "issue_id": bug, "originator_user_id": testUserID, "accountable_user_id": testUserID, "status": "running"})
	resolve := func(repo, missing string) map[string]any {
		t.Helper()
		req := withURLParam(newRequestAs(testUserID, "POST", "/", map[string]any{"repo": "git@gitee.com:example/" + repo + ".git", "candidates": []string{"feature/v3.1609_一二期", "feature/v3.1609_merge"}, "missing_decision": missing}), "id", bug)
		testutil.WithHeaders(req, "X-Workspace-ID", ws, "X-Actor-Source", "task_token", "X-Agent-ID", expert, "X-Task-ID", task)
		var out map[string]any
		testutil.Call(t, testHandler.ResolveSyndraBranch, req).Want(200).JSON(&out)
		return out
	}
	first := resolve("backend", "")
	second := resolve("web", "")
	if first["reply_on_bug"] != true || resolve("backend", "")["wakeup_id"] != first["wakeup_id"] {
		t.Fatal(first)
	}
	if f.Count(t, "SELECT count(*) FROM issue WHERE id=$1 AND assignee_id IS NULL", first["confirmation_issue_id"]) != 1 || f.Count(t, "SELECT count(*) FROM inbox_item WHERE issue_id=$1 AND recipient_id=$2 AND details->>'reason'='syndra_branch_confirmation'", bug, testUserID) != 2 {
		t.Fatal("expected unassigned anchors and one prompt per scope on original bug")
	}
	post := func(user, issue, content string, want int) {
		req := withURLParam(newRequestAs(user, "POST", "/", map[string]any{"content": content}), "id", issue)
		req.Header.Set("X-Workspace-ID", ws)
		testutil.Call(t, testHandler.CreateComment, req).Want(want)
	}
	post(testUserID, bug, "默认 feature/v3.1609_一二期", 409)
	post(other, bug, "默认 gitee.com/example/backend feature/v3.1609_一二期", 409)
	post(testUserID, bug, `默认 gitee.com/example/backend feature/v3.1609\_一二期`, 201)
	if r := resolve("backend", ""); r["state"] != "personal_default" || r["branch"] != "feature/v3.1609_一二期" {
		t.Fatal(r)
	}

	inst := engine.ResolvedInstallation{WorkspaceID: parseUUID(ws), ID: parseUUID(uuid.NewString()), AgentID: parseUUID(expert), Active: true}
	msg := channel.InboundMessage{MessageID: "om_fixture", Source: channel.Source{ChatType: channel.ChatTypeP2P, ChannelType: channel.TypeFeishu}, Type: channel.MsgTypeText, CommandText: "确认分支 QTB-1974 gitee.com/example/web feature/v3.1609_merge"}
	confirm := func(user string) string {
		t.Helper()
		msg.Raw, _ = json.Marshal(lark.InboundMessage{MessageType: "text", CommandBody: msg.CommandText})
		reply, err := testHandler.SyndraFeishuCommand(ctx, inst, engine.ResolvedIdentity{UserID: parseUUID(user)}, msg)
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	msg.Source.ChatType = channel.ChatTypeGroup
	if r := confirm(testUserID); r != "" {
		t.Fatalf("consumed group command: %s", r)
	}
	msg.Source.ChatType = channel.ChatTypeP2P
	msg.HasSelectedContext = true
	msg.Raw, _ = json.Marshal(lark.InboundMessage{MessageType: "merge_forward", CommandBody: msg.CommandText})
	if reply, err := testHandler.SyndraFeishuCommand(ctx, inst, engine.ResolvedIdentity{UserID: parseUUID(testUserID)}, msg); err != nil || reply != "" {
		t.Fatal("accepted forwarded instructions", err)
	}
	if r := confirm(other); strings.Contains(r, "已在") {
		t.Fatal("accepted another person")
	}
	for j := 0; j < 2; j++ {
		if r := confirm(testUserID); !strings.Contains(r, "已在 QTB-1974") {
			t.Fatal(r)
		}
	}
	if r := resolve("web", ""); r["branch"] != "feature/v3.1609_merge" {
		t.Fatal(r)
	}
	if n := f.Count(t, "SELECT count(*) FROM comment WHERE issue_id=$1 AND author_id=$2 AND content LIKE '%gitee.com/example/web%'", bug, testUserID); n != 1 {
		t.Fatalf("replayed comments: %d", n)
	}
	// A manually closed legacy ticket is not proof. Rebuild only from a genuine
	// member's escaped reply, never from the agent's summary.
	f.Exec(t, "UPDATE issue SET metadata='{}' WHERE id=$1", second["confirmation_issue_id"])
	f.Comment(t, second["confirmation_issue_id"].(string), `默认 feature/v3.1609\_历史`, testutil.Cols{"author_type": "member", "author_id": testUserID})
	if r := resolve("web", ""); r["branch"] != "feature/v3.1609_历史" {
		t.Fatal(r)
	}
	f.Exec(t, "UPDATE issue SET status='blocked' WHERE id=$1", bug)
	f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1", task)
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE workspace_id=$1", ws)
	svc := service.IssueWakeupService{Tasks: testHandler.TaskService}
	if err := svc.TickWorkspaces(ctx, parseUUID(ws)); err != nil {
		t.Fatal(err)
	}
	if n := f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status='queued'", bug); n < 1 {
		t.Fatal("confirmation did not resume bug")
	}
	// If the old rule already fired and its run ended without a receipt,
	// one new verified reply must suffice (without manually reopening the ticket).
	f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE issue_id=$1 AND status='queued'", bug)
	f.Exec(t, "UPDATE issue SET metadata='{}' WHERE id=$1", first["confirmation_issue_id"])
	// Exercise the consumed (not operator-disabled) legacy rule explicitly.
	f.Exec(t, "UPDATE issue_wakeup SET enabled=false,last_task_id=$2,disabled_at=NULL WHERE id=$1", first["wakeup_id"], task)
	msg.MessageID = "om_after_legacy"
	msg.CommandText = "确认分支 QTB-1974 gitee.com/example/backend feature/v3.1609_一二期"
	if r := confirm(testUserID); !strings.Contains(r, "已在 QTB-1974") {
		t.Fatal(r)
	}
	if n := f.Count(t, "SELECT count(*) FROM issue_wakeup WHERE issue_id=$1 AND condition->>'issue_id'=$2 AND enabled", bug, first["confirmation_issue_id"]); n != 1 {
		t.Fatalf("legacy wait replacements: %d", n)
	}

	box, err := secretbox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	installs, err := lark.NewInstallationService(testHandler.Queries, box)
	if err != nil {
		t.Fatal(err)
	}
	bot, err := installs.Upsert(ctx, lark.InstallationParams{WorkspaceID: parseUUID(ws), AgentID: parseUUID(expert), InstallerUserID: parseUUID(testUserID), AppID: "cli_fixture", AppSecret: "fixture", BotOpenID: "ou_bot", TenantKey: "tenant_fixture"})
	if err != nil {
		t.Fatal(err)
	}
	api := &recordingBugBot{}
	n := SyndraExpertNotifications{Handler: &Handler{Queries: testHandler.Queries, DB: testHandler.DB, LarkInstallations: installs, LarkAPIClient: api}}
	if ok, err := n.MatchesIssue(ctx, parseUUID(ws), parseUUID(first["confirmation_issue_id"].(string))); ok || err != nil {
		t.Fatal("routed non-bug", err)
	}
	f.Insert(t, "user_external_identity", testutil.Cols{"user_id": testUserID, "provider": "lark", "tenant_key": "tenant_fixture", "open_id": "ou_login_only", "external_user_id": "tenant_user", "union_id": "on_same_user"})
	message := notifications.NotificationMessage{WorkspaceID: ws, RecipientUserID: testUserID, IssueID: bug, InboxItemID: uuid.NewString(), IssueIdentifier: "QTB-1974", Body: "请确认分支", RecipientExternal: "ou_login_only"}
	send := func(kind, recipient string) {
		t.Helper()
		if err := n.Send(ctx, message); err != nil {
			t.Fatal(err)
		}
		p := api.sent[len(api.sent)-1]
		if p.RecipientIDType != kind || p.RecipientID != recipient || p.InstallationID.AppID != bot.AppID || p.IdempotencyKey != message.InboxItemID {
			t.Fatalf("unexpected delivery to %s:%s", p.RecipientIDType, p.RecipientID)
		}
	}
	send("user_id", "tenant_user")
	f.Exec(t, "UPDATE channel_installation SET config=config-'tenant_key' WHERE id=$1", bot.ID)
	send("union_id", "on_same_user")
	f.Insert(t, "channel_user_binding", testutil.Cols{"workspace_id": ws, "channel_type": "feishu", "installation_id": bot.ID, "multica_user_id": testUserID, "channel_user_id": "ou_expert"})
	send("open_id", "ou_expert")
	t.Run("existing inbox delivery queue", func(t *testing.T) {
		bus := events.New()
		d := notifications.NewDispatcher(testHandler.Queries, notifications.Config{Enabled: true, PollInterval: time.Second, BatchSize: 5, MaxAttempts: 2}, n)
		d.RegisterInboxListener(bus)
		item := first["wakeup_id"].(string)
		f.Cleanup(t, "DELETE FROM notification_delivery WHERE inbox_item_id=$1", item)
		event := events.Event{WorkspaceID: ws, Type: protocol.EventInboxNew, Payload: map[string]any{"item": map[string]any{"id": item, "type": "agent_blocked", "issue_id": &bug, "workspace_id": ws, "recipient_type": "member", "recipient_id": testUserID}}}
		bus.Publish(event)
		bus.Publish(event)
		if f.Count(t, "SELECT count(*) FROM notification_delivery WHERE inbox_item_id=$1 AND status='pending'", item) != 1 {
			t.Fatal("missing or duplicate expert delivery")
		}
		wctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); d.RunWorker(wctx) }()
		defer func() { cancel(); <-done }()
		deadline := time.Now().Add(5 * time.Second)
		for f.Count(t, "SELECT count(*) FROM notification_delivery WHERE inbox_item_id=$1 AND status='sent'", item) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("expert prompt was not sent")
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		<-done
		if !strings.Contains(api.sent[len(api.sent)-1].Text, "确认分支 QTB-1974 gitee.com/example/backend") {
			t.Fatal("lost original issue and repository in prompt")
		}
	})
	t.Run("local instruction migration preserves surrounding text", func(t *testing.T) {
		const before = "其他说明。本人分支确认沿用 Multica 原确认单，已有个人版本分支选择继续复用。结束。"
		f.Exec(t, "UPDATE agent SET instructions=$2 WHERE id=$1", expert, before)
		tx, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, direction := range []string{"up", "up", "down"} {
			sql, err := os.ReadFile("../../migrations/900003_local_syndra_expert_branch_instructions." + direction + ".sql")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				t.Fatal(err)
			}
			var actual string
			if err := tx.QueryRow(ctx, "SELECT instructions FROM agent WHERE id=$1", expert).Scan(&actual); err != nil {
				t.Fatal(err)
			}
			if direction == "up" && (!strings.HasPrefix(actual, "其他说明。") || !strings.HasSuffix(actual, "结束。") || strings.Count(actual, "确认分支 原Bug编号") != 1) || direction == "down" && actual != before {
				t.Fatal(actual)
			}
		}
	})
}
