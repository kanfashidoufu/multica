package externalissue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestBugAutomationPilotEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, provider, assignee, fallback string
		want                               bool
	}{
		{"pilot", "syndra", "王宁", "", true},
		{"trimmed", "syndra", " 王宁 ", "", true},
		{"another member", "syndra", "刘鹏", "", false},
		{"another provider", "jira", "王宁", "", false},
		{"unknown member falls back to owner", "syndra", "王宁", "assignee_not_unique_or_not_member", false},
		{"missing assignee falls back to owner", "syndra", "", "missing_assignee_name", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := BugSyncItem{Assignee: BugSyncPerson{Name: strPtr(tc.assignee)}}
			if got := bugAutomationPilotEligible(tc.provider, item, tc.fallback); got != tc.want {
				t.Fatalf("eligible=%v, want %v", got, tc.want)
			}
		})
	}
	item := BugSyncItem{BugDetail: BugSyncDetail{Assignee: BugSyncPerson{Name: strPtr("王宁")}}}
	if !bugAutomationPilotEligible("syndra", item, "") {
		t.Fatal("nested Syndra assignee should use the same resolution contract")
	}
}

func TestBugAutomationPilotEligibilityUsesConfiguredAssignees(t *testing.T) {
	item := BugSyncItem{Assignee: BugSyncPerson{Name: strPtr("刘鹏")}}
	allowed := configuredBugAutomationAssignees("王宁, 刘鹏")
	if !bugAutomationPilotEligibleForNames("syndra", item, "", allowed) {
		t.Fatal("configured assignee should be eligible")
	}
	if bugAutomationPilotEligibleForNames("syndra", item, "", configuredBugAutomationAssignees("王宁")) {
		t.Fatal("unconfigured assignee should not be eligible")
	}
	got := configuredBugAutomationAssignees("")
	if len(got) != 1 {
		t.Fatalf("default allowlist = %#v, want only %q", got, bugAutomationPilotAssignee)
	}
	if _, ok := got[bugAutomationPilotAssignee]; !ok {
		t.Fatalf("default allowlist = %#v, want %q", got, bugAutomationPilotAssignee)
	}
}

func TestBugAutomationMirrorPreservesOnlyEnrolledDeliveryState(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		for _, currentStatus := range []string{"blocked", "in_progress", "in_review", "done"} {
			existingMetadata, _ := json.Marshal(map[string]any{bugAutomationMetadataKey: enrolled})
			metadata := map[string]any{bugAutomationMetadataKey: true, "bug_status": "active"}
			description, status, raw, err := bugAutomationMirror(db.Issue{Status: currentStatus, Metadata: existingMetadata}, "source", bugSyncState{status: "todo"}, metadata)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if enrolled {
				if status != currentStatus || got[bugAutomationMetadataKey] != true || !strings.Contains(description, bugAutomationAcceptance) {
					t.Fatalf("enrolled mirror lost delivery state: %s %s %s", status, description, raw)
				}
			} else if status != "todo" || description != "source" || got[bugAutomationMetadataKey] != nil {
				t.Fatalf("source metadata enrolled an ordinary bug: %s %s %s", status, description, raw)
			}
		}
	}
}

func TestBugAutomationMirrorDoesNotOverflowMetadata(t *testing.T) {
	metadata := make(map[string]any)
	for n := 0; n < maxBugMetadataKeys; n++ {
		metadata[fmt.Sprintf("key_%d", n)] = "value"
	}
	existing := db.Issue{Status: "blocked", Metadata: []byte(`{"multica_bug_automation":true}`)}
	if _, _, _, err := bugAutomationMirror(existing, "source", bugSyncState{status: "todo"}, metadata); err == nil {
		t.Fatal("full mirror must not erase enrollment or exceed metadata capacity")
	}
	delete(metadata, "key_0")
	delete(metadata, "key_1")
	metadata["source"], metadata["external_source"] = "syndra", "syndra"
	_, status, _, err := bugAutomationMirror(existing, "source", bugSyncState{status: "todo"}, metadata)
	if err != nil || status != "blocked" || len(metadata) != maxBugMetadataKeys || metadata[bugAutomationMetadataKey] != true || metadata["external_source"] != "syndra" {
		t.Fatalf("failed to preserve enrollment on full Syndra snapshot: %v", err)
	}
}

func TestBugAutomationImportPilotAndRepeatedSync(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	pilot := createImporterWorkspaceMember(t, ctx, pool, q, fx.Workspace.ID, "王宁", "")
	other := createImporterWorkspaceMember(t, ctx, pool, q, fx.Workspace.ID, "刘鹏", "")
	pilotAgent := createImporterSharedExpert(t, ctx, pool, q, fx.Workspace.ID, fx.User.ID)
	createImporterOwnedAgent(t, ctx, pool, q, fx.Workspace.ID, pilot.User.ID, "Pilot")
	createImporterOwnedAgent(t, ctx, pool, q, fx.Workspace.ID, other.User.ID, "Other")
	bus := events.New()
	tasks := service.NewTaskService(q, pool, nil, bus)
	importer := &Importer{Queries: q, IssueService: service.NewIssueService(q, pool, bus, analytics.NoopClient{}, tasks), Bus: bus, Config: Config{WebhookToken: "test-token", BugWorkspaceID: util.UUIDToString(fx.Workspace.ID)}}
	for _, tc := range []struct {
		name, status string
		agent        bool
	}{
		{"王宁", "active", true}, {"刘鹏", "active", false}, {"王宁", "resolved", false}, {"missing", "active", false},
	} {
		t.Run(tc.name+tc.status, func(t *testing.T) {
			request := BugSyncRequest{Payload: BugSyncPayload{Source: "syndra", SourceEnv: "local", Items: []BugSyncItem{{
				ExternalKey: "pilot-" + tc.name + tc.status, BugID: 42, VersionName: "v2.91.56", Title: "Bug", Status: tc.status,
				Assignee: BugSyncPerson{Name: strPtr(tc.name)}, Metadata: map[string]any{bugAutomationMetadataKey: true},
			}}}}
			result, err := importer.ImportBugSync(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			issue := result.Items[0].Issue
			if (issue.AssigneeType.String == "agent") != tc.agent {
				t.Fatalf("unexpected assignee: %v", issue.AssigneeType)
			}
			queued, err := q.ListTasksByIssue(ctx, issue.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.agent {
				if len(queued) != 0 || strings.Contains(string(issue.Metadata), bugAutomationMetadataKey) || strings.Contains(issue.Description.String, bugAutomationAcceptance) {
					t.Fatalf("nonpilot was enrolled: tasks=%d metadata=%s", len(queued), issue.Metadata)
				}
				return
			}
			if issue.AssigneeID != pilotAgent.ID || len(queued) != 1 || queued[0].AccountableUserID != pilot.User.ID {
				t.Fatal("pilot task lost agent or accountable human")
			}
			if !strings.Contains(issue.Description.String, bugAutomationAcceptance) ||
				!strings.Contains(issue.Description.String, "合入 test") ||
				!strings.Contains(issue.Description.String, "验证失败时提交当前修复快照") {
				t.Fatal("missing CI and integration acceptance criteria")
			}
			_, err = q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Status: "blocked"})
			if err != nil {
				t.Fatal(err)
			}
			// The importer found the row before the agent blocked it. The update
			// must use the locked current state rather than that old snapshot.
			previous, mirrored, err := importer.updateBugSyncMirror(ctx, issue, bugDeveloperAssignment{Reviewer: db.Member{UserID: pilot.User.ID}}, issue.Title, "source", bugSyncState{status: "done"}, issue.Priority, map[string]any{"external_source": "syndra"})
			if err != nil || previous.Status != "blocked" || mirrored.Status != "blocked" {
				t.Fatalf("stale snapshot overwrote agent state: %s %s %v", previous.Status, mirrored.Status, err)
			}
			request.Payload.Items[0].Status = "resolved"
			result, err = importer.ImportBugSync(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			updated := result.Items[0].Issue
			if !result.Items[0].Existing || updated.Status != "done" || updated.AssigneeID != pilotAgent.ID || strings.Count(updated.Description.String, bugAutomationAcceptance) != 1 {
				t.Fatal("resolved Syndra bug must complete the enrolled issue")
			}
			queued, err = q.ListTasksByIssue(ctx, issue.ID)
			if err != nil || len(queued) != 1 || queued[0].Status != "cancelled" {
				t.Fatalf("upsert duplicated task: %d %v", len(queued), err)
			}
		})
	}
}

func TestBugAgentEnqueueFailureReturnsBugToHuman(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	fixture := dbfx.New(pool, util.UUIDToString(fx.Workspace.ID), util.UUIDToString(fx.User.ID))
	// The inventory may have been read just before an agent was archived.
	agentID := fixture.Agent(t, "Archived", "", dbfx.Cols{"archived_at": dbfx.Raw("now()")})
	issueID := fixture.Issue(t, "Bug", dbfx.Cols{"assignee_type": "member", "assignee_id": fx.User.ID})
	parse := func(s string) pgtype.UUID {
		id, err := util.ParseUUID(s)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	issue, err := q.GetIssue(ctx, parse(issueID))
	if err != nil {
		t.Fatal(err)
	}
	bus := events.New()
	importer := &Importer{Queries: q, IssueService: service.NewIssueService(q, pool, bus, analytics.NoopClient{}, service.NewTaskService(q, pool, nil, bus))}
	result, err := importer.routeBugToExpert(ctx, issue, bugDeveloperAssignment{Reviewer: db.Member{UserID: fx.User.ID}, DeveloperAgentID: parse(agentID)})
	if err == nil || result.AssigneeID != fx.User.ID || result.AssigneeType.String != "member" {
		t.Fatalf("failed enqueue abandoned bug on agent: %v %v", result.AssigneeType, err)
	}
}

func TestSyndraLegacyBugRoutingOnUpsert(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	human := createImporterWorkspaceMember(t, ctx, pool, q, fx.Workspace.ID, "凌情", "")
	f := dbfx.New(pool, util.UUIDToString(fx.Workspace.ID), util.UUIDToString(human.User.ID))
	personal := createImporterOwnedAgent(t, ctx, pool, q, fx.Workspace.ID, human.User.ID, "个人助手")
	expert := createImporterSharedExpert(t, ctx, pool, q, fx.Workspace.ID, fx.User.ID)
	bus := events.New()
	i := &Importer{Queries: q, IssueService: service.NewIssueService(q, pool, bus, analytics.NoopClient{}, service.NewTaskService(q, pool, nil, bus)), Bus: bus, Config: Config{WebhookToken: "fixture", BugWorkspaceID: f.WorkspaceID}}
	for _, tc := range []struct {
		name, local, source, oldRun string
		migrate                     bool
	}{
		{"closed source while waiting for branch", "blocked", "closed", "completed", false},
		{"closed source with local unfinished delivery", "in_progress", "closed", "completed", false},
		{"open bug with an idle legacy executor", "todo", "active", "completed", true},
		{"unlisted owner", "todo", "active", "completed", false},
		{"legacy run still running", "in_progress", "active", "running", false},
		{"deferred legacy run", "todo", "active", "deferred", false},
		{"waiting for review", "in_review", "active", "completed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i.Config.BugAutomationAssignees = "凌情"
			if tc.name == "unlisted owner" {
				i.Config.BugAutomationAssignees = "王宁"
			}
			key := "test:" + tc.name
			id := f.Issue(t, tc.name, dbfx.Cols{"origin_type": OriginType, "origin_id": sourceOriginID("syndra", "prod", "version_bug", key), "status": tc.local, "metadata": []byte(`{"multica_bug_automation":true,"bug_assignee_name":"凌情","sync_source_env":"prod","bug_version_id":42}`), "assignee_type": "agent", "assignee_id": personal.ID})
			old := f.Task(t, util.UUIDToString(personal.ID), dbfx.Cols{"runtime_id": personal.RuntimeID, "issue_id": id, "status": tc.oldRun})
			req := BugSyncRequest{Payload: BugSyncPayload{Source: "syndra", SourceEnv: "prod", Items: []BugSyncItem{{BugID: 42, ExternalKey: key, VersionID: 42, Title: "Bug", Status: tc.source, Assignee: BugSyncPerson{Name: strPtr("凌情")}}}}}
			// Two webhook workers may deliver the same active source snapshot.
			type outcome struct {
				res BugSyncResult
				err error
			}
			results := make(chan outcome, 2)
			for j := 0; j < 2; j++ {
				go func() { res, err := i.ImportBugSync(ctx, req); results <- outcome{res, err} }()
			}
			for j := 0; j < 2; j++ {
				got := <-results
				res, err := got.res, got.err
				if err != nil {
					t.Fatal(err)
				}
				issue := res.Items[0].Issue
				want := personal.ID
				if tc.migrate {
					want = expert.ID
				}
				var m map[string]any
				if err := json.Unmarshal(issue.Metadata, &m); err != nil {
					t.Fatal(err)
				}
				wantStatus := tc.local
				if tc.source == "closed" {
					wantStatus = "done"
				}
				if issue.AssigneeID != want || issue.Status != wantStatus || m["bug_status"] != tc.source || !res.Items[0].Existing {
					t.Fatalf("wrong mirror or route: agent=%s local=%s source=%v", util.UUIDToString(issue.AssigneeID), issue.Status, m["bug_status"])
				}
			}
			wantCount := 0
			if tc.migrate {
				wantCount = 1
			}
			if n := f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2", id, expert.ID); n != wantCount {
				t.Fatalf("expert runs=%d want=%d", n, wantCount)
			}
			if n := f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status=$2 AND agent_id=$3", old, tc.oldRun, personal.ID); n != 1 {
				t.Fatal("legacy run changed")
			}
		})
	}
}

func TestBugRoutingRequiresWorkspaceGrantForNamedExpert(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	f := dbfx.New(pool, util.UUIDToString(fx.Workspace.ID), util.UUIDToString(fx.User.ID))
	createImporterOwnedAgent(t, ctx, pool, q, fx.Workspace.ID, fx.User.ID, "个人开发助手")
	i := Importer{Queries: q}
	count := func(want int) {
		t.Helper()
		got := i.listWorkspaceBugExperts(ctx, BugSyncPayload{}, fx.Workspace.ID)
		if len(got) != want {
			t.Fatalf("workspace bug experts=%d, want %d", len(got), want)
		}
	}
	count(0)
	expert := createImporterOwnedAgent(t, ctx, pool, q, fx.Workspace.ID, fx.User.ID, BugExpertName)
	count(0)
	f.Exec(t, "UPDATE agent SET visibility='workspace', permission_mode='public_to' WHERE id=$1", expert.ID)
	f.Insert(t, "agent_invocation_target", dbfx.Cols{"agent_id": expert.ID, "target_type": "member", "target_id": fx.User.ID})
	count(0)
	f.Insert(t, "agent_invocation_target", dbfx.Cols{"agent_id": expert.ID, "target_type": "workspace", "target_id": fx.Workspace.ID})
	count(1)
	f.Exec(t, "UPDATE agent SET runtime_id=NULL WHERE id=$1", expert.ID)
	count(0)
}

func TestSyndraIdleMemberBugHandoffRollsBackIfEnqueueFails(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL database")
	}
	pool := dbfx.IsolatedDB(t, dsn)
	q := db.New(pool)
	ctx := t.Context()
	fx := createImporterFixture(t, ctx, pool, q)
	human := createImporterWorkspaceMember(t, ctx, pool, q, fx.Workspace.ID, "试用负责人", "")
	f := dbfx.New(pool, util.UUIDToString(fx.Workspace.ID), util.UUIDToString(human.User.ID))
	expert := createImporterSharedExpert(t, ctx, pool, q, fx.Workspace.ID, fx.User.ID)
	bus := events.New()
	i := Importer{Queries: q, Bus: bus, IssueService: service.NewIssueService(q, pool, bus, analytics.NoopClient{}, service.NewTaskService(q, pool, nil, bus)), Config: Config{WebhookToken: "test", BugWorkspaceID: f.WorkspaceID}}
	req := BugSyncRequest{Payload: BugSyncPayload{Source: "syndra", SourceEnv: "test", Items: []BugSyncItem{{BugID: 32, EntityType: "version_bug", Title: "waiting for enrollment", Status: "active", Assignee: BugSyncPerson{Name: strPtr("试用负责人")}}}}}
	r, err := i.ImportBugSync(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	id := r.Items[0].Issue.ID
	if r.Items[0].Issue.AssigneeID != human.User.ID || f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", id) != 0 {
		t.Fatal("unlisted member dispatched")
	}
	i.Config.BugAutomationAssignees = "试用负责人"
	// Fail the native queue INSERT *after* the assignment callback has run.
	f.Exec(t, `CREATE FUNCTION reject_test_enqueue() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test enqueue rejected'; END; $$`)
	f.Exec(t, "CREATE TRIGGER reject_test_enqueue BEFORE INSERT ON agent_task_queue FOR EACH ROW EXECUTE FUNCTION reject_test_enqueue()")
	if _, err := i.ImportBugSync(ctx, req); err == nil {
		t.Fatal("expected enqueue failure")
	}
	saved, err := q.GetIssue(ctx, id)
	if err != nil || saved.AssigneeType.String != "member" || saved.AssigneeID != human.User.ID || bugAutomationEnrolled(saved.Metadata) || strings.Contains(saved.Description.String, bugAutomationAcceptance) || f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", id) != 0 {
		t.Fatalf("partial handoff after rejected enqueue: %v", err)
	}
	f.Exec(t, "DROP TRIGGER reject_test_enqueue ON agent_task_queue")
	for j := 0; j < 2; j++ {
		r, err = i.ImportBugSync(ctx, req)
		if err != nil || !r.Items[0].Existing || r.Items[0].Issue.AssigneeID != expert.ID || !bugAutomationEnrolled(r.Items[0].Issue.Metadata) {
			t.Fatalf("retry did not hand off old member bug: %v", err)
		}
	}
	if f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND accountable_user_id=$3", id, expert.ID, human.User.ID) != 1 {
		t.Fatal("expected exactly one owned expert run")
	}
}
