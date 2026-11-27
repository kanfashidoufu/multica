package externalissue

import (
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestOnlyExplicitSyndraTerminalStatesStopWork(t *testing.T) {
	for _, tc := range []struct {
		provider string
		item     BugSyncItem
		stop     bool
	}{
		{"syndra", BugSyncItem{Status: " CLOSED "}, true},
		{"syndra", BugSyncItem{StatusName: "已关闭"}, true},
		{"syndra", BugSyncItem{BugDetail: BugSyncDetail{Status: "closed"}}, true},
		{"syndra", BugSyncItem{Status: "resolved", StatusName: "已解决"}, true},
		{"syndra", BugSyncItem{StatusName: "已解决"}, true},
		{"syndra", BugSyncItem{Status: "unresolved", Metadata: map[string]any{"bug_status": "resolved"}}, false},
		{"syndra", BugSyncItem{Status: "active", StatusName: "已关闭"}, false},
		{"syndra", BugSyncItem{StatusName: "未关闭", Metadata: map[string]any{"bug_status": "closed"}}, false},
		{"other", BugSyncItem{Status: "closed"}, false},
	} {
		got := bugStateForSync(tc.provider, tc.item)
		if got.terminalBySyndra != tc.stop || tc.stop && got.status != "done" {
			t.Errorf("%+v => %+v", tc, got)
		}
	}
}

func TestSyndraTerminalSyncCompletesAndStopsOnlyThisBug(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	ws := util.UUIDToString(fx.Workspace.ID)
	f := testutil.New(pool, ws, util.UUIDToString(fx.User.ID))
	expert := createImporterSharedExpert(t, ctx, pool, q, fx.Workspace.ID, fx.User.ID)
	for _, name := range []string{"王宁", "非试用成员"} {
		createImporterWorkspaceMember(t, ctx, pool, q, fx.Workspace.ID, name, "")
		for _, terminal := range []string{"closed", "resolved"} {
			t.Run(name+"/"+terminal, func(t *testing.T) {
				bus := events.New()
				i := Importer{Queries: q, Bus: bus, IssueService: service.NewIssueService(q, pool, bus, analytics.NoopClient{}, service.NewTaskService(q, pool, nil, bus)), Config: Config{WebhookToken: "fixture", BugWorkspaceID: ws}}
				req := BugSyncRequest{Payload: BugSyncPayload{Source: "syndra", SourceEnv: "test", Items: []BugSyncItem{{BugID: 42, ExternalKey: "close:" + name + terminal, EntityType: "version_bug", Title: "closed fixture", Status: "active", UpdatedAt: "2026-11-27T16:00:00+08:00", Assignee: BugSyncPerson{Name: strPtr(name)}}}}}
				push := func() db.Issue {
					t.Helper()
					r, err := i.ImportBugSync(ctx, req)
					if err != nil {
						t.Fatal(err)
					}
					return r.Items[0].Issue
				}
				bug := push()
				if bugAutomationEnrolled(bug.Metadata) != (name == "王宁") {
					t.Fatal("unexpected whitelist enrollment")
				}
				id := util.UUIDToString(bug.ID)
				f.Exec(t, "UPDATE issue SET status='blocked' WHERE id=$1", id)
				pid := f.Runtime(t, "close fixture", testutil.Cols{"provider": "pi"})
				wake := f.Insert(t, "issue_wakeup", testutil.Cols{"id": uuid.NewString(), "workspace_id": ws, "issue_id": id, "created_by": f.UserID, "agent_id": expert.ID, "kind": "event", "mode": "once", "instruction": "branch wait", "condition": `{"type":"other_issue","state":"done"}`})
				active := map[string]string{}
				for _, state := range []string{"queued", "dispatched", "running", "waiting_local_directory", "deferred"} {
					agent := f.Agent(t, "close fixture "+state, pid)
					row := testutil.Cols{"runtime_id": pid, "issue_id": id, "status": state}
					if state == "running" {
						row["started_at"] = testutil.Raw("now()")
						row["result"] = `{"process_cleanup_capability":"pi-process-cleanup-v1"}`
					}
					active[state] = f.Task(t, agent, row)
				}
				other := f.Issue(t, "unrelated", nil)
				otherTask := f.Task(t, util.UUIDToString(expert.ID), testutil.Cols{"issue_id": other, "runtime_id": expert.RuntimeID, "status": "running"})
				history := f.Task(t, util.UUIDToString(expert.ID), testutil.Cols{"issue_id": id, "runtime_id": expert.RuntimeID, "status": "completed", "result": `{"comment":"keep"}`})
				notices := map[string]int{}
				bus.Subscribe(protocol.EventTaskCancelled, func(e events.Event) { notices[e.TaskID]++ })
				req.Payload.Items[0].Status = terminal
				req.Payload.Items[0].UpdatedAt = "2026-11-27T15:00:00+08:00"
				if stale := push(); stale.Status != "blocked" || len(notices) != 0 {
					t.Fatal("older closure cancelled current work")
				}
				req.Payload.Items[0].UpdatedAt = "2026-11-27T17:00:00+08:00"
				for j := 0; j < 2; j++ {
					got := push()
					if got.Status != "done" || got.ID != bug.ID {
						t.Fatalf("close returned %s %v", got.Status, got.ID)
					}
				}
				for state, task := range active {
					if n := f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='cancelled' AND cancelled_by_type='system' AND completed_at IS NOT NULL", task); n != 1 || notices[task] != 1 {
						t.Errorf("%s not stopped exactly once (%d events)", state, notices[task])
					}
				}
				if f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE id=$1 AND result->>'process_cleanup_pending'='true'", active["running"]) != 1 {
					t.Error("missing daemon cleanup fence")
				}
				if f.Count(t, "SELECT count(*) FROM issue_wakeup WHERE id=$1 AND NOT enabled AND disabled_at IS NOT NULL", wake) != 1 {
					t.Error("branch wait not disabled")
				}
				if f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='running'", otherTask) != 1 || notices[otherTask] != 0 {
					t.Error("cancelled unrelated bug")
				}
				if f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='completed' AND result->>'comment'='keep'", history) != 1 || notices[history] != 0 {
					t.Error("altered historical run")
				}
				if f.Count(t, "SELECT count(*) FROM local_syndra_bug_completion WHERE issue_id=$1", id) != 0 {
					t.Error("source closure echoed back to Syndra")
				}
			})
		}
	}
}

func TestSyndraTerminalSnapshotWinsAgainstConcurrentTakeover(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	human := createImporterWorkspaceMember(t, ctx, pool, q, fx.Workspace.ID, "试用负责人", "")
	f := testutil.New(pool, util.UUIDToString(fx.Workspace.ID), util.UUIDToString(human.User.ID))
	createImporterSharedExpert(t, ctx, pool, q, fx.Workspace.ID, fx.User.ID)
	bus := events.New()
	i := Importer{Queries: q, Bus: bus, IssueService: service.NewIssueService(q, pool, bus, analytics.NoopClient{}, service.NewTaskService(q, pool, nil, bus)), Config: Config{WebhookToken: "fixture", BugWorkspaceID: f.WorkspaceID, BugAutomationAssignees: "试用负责人"}}
	key := "syndra:test:version_bug:42"
	id := f.Issue(t, "old bug", testutil.Cols{"origin_type": OriginType, "origin_id": sourceOriginID("syndra", "test", "version_bug", key), "assignee_type": "member", "assignee_id": f.UserID})
	start := make(chan struct{})
	results := make(chan error, 3)
	for _, state := range []string{"active", "resolved", "active"} {
		go func(state string) {
			<-start
			at := "2026-11-27T16:00:00+08:00"
			if state == "resolved" {
				at = "2026-11-27T16:01:00+08:00"
			}
			_, err := i.ImportBugSync(ctx, BugSyncRequest{Payload: BugSyncPayload{Source: "syndra", SourceEnv: "test", Items: []BugSyncItem{{BugID: 42, ExternalKey: key, EntityType: "version_bug", Title: "old bug", Status: state, UpdatedAt: at, Assignee: BugSyncPerson{Name: strPtr("试用负责人")}}}}})
			results <- err
		}(state)
	}
	close(start)
	for j := 0; j < 3; j++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if f.Count(t, "SELECT count(*) FROM issue WHERE id=$1 AND status='done' AND metadata->>'bug_status'='resolved'", id) != 1 {
		t.Fatal("old active snapshot overwrote completion")
	}
	if f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status<>'cancelled'", id) != 0 || f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", id) > 1 {
		t.Fatal("duplicate or surviving run after terminal snapshot")
	}
}
