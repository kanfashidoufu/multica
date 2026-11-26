package externalissue

import (
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"testing"
)

func TestBranchScopeIdentity(t *testing.T) {
	s := BranchScope{"workspace", "prod", "github.com/org/repo", 123, "member-a"}
	for _, raw := range []string{"https://GitHub.com/Org/Repo.git", "git@github.com:org/repo.git", "ssh://git@github.com:22/org/repo"} {
		key, err := canonicalBranchRepo(raw)
		if err != nil || key != s.Repo {
			t.Errorf("unexpected repo key %q: %v", key, err)
		}
	}
	for _, raw := range []string{"", "https://token:secret@example.test/org/repo", "https://example.test/org/repo?token=x", "file:///usr/src/repo", "https://example.test/org/../repo"} {
		if _, err := canonicalBranchRepo(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, mutate := range []func(*BranchScope){
		func(v *BranchScope) { v.Workspace = "other" }, func(v *BranchScope) { v.Env = "test" },
		func(v *BranchScope) { v.Repo = "github.com/other/repo" }, func(v *BranchScope) { v.Version++ }, func(v *BranchScope) { v.Member = "member-b" },
	} {
		v := s
		mutate(&v)
		if v.Marker() == s.Marker() {
			t.Errorf("scope collided: %+v", v)
		}
	}
}

func TestSyndraBranchScopeUsesStructuredMirror(t *testing.T) {
	person := "王宁"
	item := BugSyncItem{BugDetail: BugSyncDetail{Assignee: BugSyncPerson{Name: &person}, Version: BugSyncVersion{VersionID: 42, VersionName: "v3.12.0"}}, Metadata: map[string]any{"bug_assignee_name": "another", "bug_version_id": 99, "sync_source_env": "test"}}
	m := bugIssueMetadata(BugSyncPayload{SourceEnv: "prod"}, item, "syndra", "key", "")
	if m["bug_assignee_name"] != person || m["bug_version_id"] != int64(42) || m["sync_source_env"] != "prod" || m["bug_version_name"] != "v3.12.0" {
		t.Fatalf("incorrect branch scope: %#v", m)
	}
}

func TestBranchWaitWithdrawalOnSyndraReassignment(t *testing.T) {
	t.Run("owner confirmation", func(t *testing.T) { testBranchHandoff(t, "in_review", "review_requested") })
	t.Run("technical blocker", func(t *testing.T) { testBranchHandoff(t, "blocked", "agent_blocked") })
}

func TestV2BranchScopeDetectsSameNameReassignment(t *testing.T) {
	before := []byte(`{"sync_schema_version":"syndra.multica.version_bug.webhook.v2","bug_version_id":42,"bug_assignee_name":"王宁","bug_assignee_syndra_user_id":"9223372036854775807","bug_verified_member_id":"first"}`)
	after := []byte(`{"sync_schema_version":"syndra.multica.version_bug.webhook.v2","bug_version_id":42,"bug_assignee_name":"王宁","bug_assignee_syndra_user_id":"17","bug_verified_member_id":"second"}`)
	if !sourceBranchScopeChanged(before, after) || sourceBranchScopeChanged(before, before) {
		t.Fatal("v2 branch decisions must follow stable identity")
	}
}

func testBranchHandoff(t *testing.T, status, noticeType string) {
	ctx := t.Context()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	newOwner := createImporterWorkspaceMember(t, ctx, pool, q, fx.Workspace.ID, "新成员", "")
	f := testutil.New(pool, util.UUIDToString(fx.Workspace.ID), util.UUIDToString(fx.User.ID))
	bug := f.Issue(t, "waiting", testutil.Cols{"status": status, "metadata": []byte(`{"multica_bug_automation":true,"bug_assignee_name":"王宁","sync_source_env":"prod","bug_version_id":42}`)})
	f.Cleanup(t, "DELETE FROM inbox_item WHERE issue_id=$1", bug)
	id, err := util.ParseUUID(bug)
	if err != nil {
		t.Fatal(err)
	}
	issue, err := q.GetIssue(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	agent := f.Agent(t, "waiter", "")
	wait := func(instruction string) string {
		return f.Insert(t, "issue_wakeup", testutil.Cols{"id": uuid.NewString(), "workspace_id": f.WorkspaceID, "issue_id": bug, "agent_id": agent, "created_by": f.UserID, "instruction": instruction, "kind": "event", "mode": "once", "condition": `{"type":"other_issue","state":"done"}`})
	}
	ours, unrelated := wait(BranchWaitInstruction+"[syndra-branch:old]"), wait("Another user rule")
	i := Importer{Queries: q, IssueService: &service.IssueService{TxStarter: pool}}
	for _, tc := range []struct {
		name    string
		enabled int
	}{{"王宁", 1}, {"新成员", 0}, {"新成员", 0}} {
		_, updated, err := i.updateBugSyncMirror(ctx, issue, db.Member{UserID: newOwner.User.ID}, "waiting", "source", "todo", "none", map[string]any{"bug_assignee_name": tc.name, "sync_source_env": "prod", "bug_version_id": 42})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Status != status {
			t.Fatalf("source sync changed %s to %s", status, updated.Status)
		}
		if got := f.Count(t, "SELECT count(*) FROM issue_wakeup WHERE id=$1 AND enabled", ours); got != tc.enabled {
			t.Fatalf("%s: enabled=%d", tc.name, got)
		}
		if got := f.Count(t, "SELECT count(*) FROM issue_wakeup WHERE id=$1 AND enabled", unrelated); got != 1 {
			t.Fatal("unrelated rule withdrawn")
		}
	}
	if n := f.Count(t, "SELECT count(*) FROM inbox_item WHERE issue_id=$1 AND recipient_id=$2 AND type=$3 AND details->>'reason'='syndra_branch_scope_changed'", bug, newOwner.User.ID, noticeType); n != 1 {
		t.Fatalf("new owner got %d %s handoffs", n, noticeType)
	}
}
