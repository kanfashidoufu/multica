package externalissue

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestBugAutomationMirrorPreservesOnlyEnrolledDeliveryState(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		for _, currentStatus := range []string{"blocked", "in_progress", "in_review", "done"} {
			existingMetadata, _ := json.Marshal(map[string]any{bugAutomationMetadataKey: enrolled})
			metadata := map[string]any{bugAutomationMetadataKey: true, "bug_status": "resolved"}
			description, status, raw, err := bugAutomationMirror(db.Issue{Status: currentStatus, Metadata: existingMetadata}, "source", "done", metadata)
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
			} else if status != "done" || description != "source" || got[bugAutomationMetadataKey] != nil {
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
	if _, _, _, err := bugAutomationMirror(existing, "source", "done", metadata); err == nil {
		t.Fatal("full mirror must not erase enrollment or exceed metadata capacity")
	}
}

func TestBugAutomationImportPilotAndRepeatedSync(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	pilot := createImporterWorkspaceMember(t, ctx, pool, q, fx.Workspace.ID, "王宁", "")
	other := createImporterWorkspaceMember(t, ctx, pool, q, fx.Workspace.ID, "刘鹏", "")
	pilotAgent := createImporterOwnedAgent(t, ctx, pool, q, fx.Workspace.ID, pilot.User.ID, "Pilot")
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
			if !strings.Contains(issue.Description.String, bugAutomationAcceptance) {
				t.Fatal("missing CI and integration acceptance criteria")
			}
			_, err = q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Status: "blocked"})
			if err != nil {
				t.Fatal(err)
			}
			// The importer found the row before the agent blocked it. The update
			// must use the locked current state rather than that old snapshot.
			previous, mirrored, err := importer.updateBugSyncMirror(ctx, issue, issue.Title, "source", "done", issue.Priority, map[string]any{"external_source": "syndra"})
			if err != nil || previous.Status != "blocked" || mirrored.Status != "blocked" {
				t.Fatalf("stale snapshot overwrote agent state: %s %s %v", previous.Status, mirrored.Status, err)
			}
			request.Payload.Items[0].Status = "resolved"
			result, err = importer.ImportBugSync(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			updated := result.Items[0].Issue
			if !result.Items[0].Existing || updated.Status != "blocked" || updated.AssigneeID != pilotAgent.ID || strings.Count(updated.Description.String, bugAutomationAcceptance) != 1 {
				t.Fatal("upsert overwrote pending version integration")
			}
			queued, err = q.ListTasksByIssue(ctx, issue.ID)
			if err != nil || len(queued) != 1 {
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
	result, err := importer.routeNewBugToDeveloperAgent(ctx, issue, bugDeveloperAssignment{ReviewerID: fx.User.ID, DeveloperAgentID: parse(agentID)})
	if err == nil || result.AssigneeID != fx.User.ID || result.AssigneeType.String != "member" {
		t.Fatalf("failed enqueue abandoned bug on agent: %v %v", result.AssigneeType, err)
	}
}
