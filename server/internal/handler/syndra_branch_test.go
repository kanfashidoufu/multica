package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/externalissue"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestSyndraDecisionReceiptIsNotPubliclyWritable(t *testing.T) {
	h := ProtectSyndraBranchReceipt(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for _, method := range []string{"PUT", "DELETE"} {
		for key, status := range map[string]int{externalissue.BranchDecisionKey: 403, "waiting_on": 204} {
			req := withURLParam(newRequest(method, "/", nil), "key", key)
			testutil.Call(t, h.ServeHTTP, req).Want(status)
		}
	}
}

func TestSyndraPersonalBranchesAndWaiters(t *testing.T) {
	t.Setenv("MULTICA_EXTERNAL_BUG_AUTOMATION_ASSIGNEES", handlerTestName+",Branch tester")
	ws := dbfx.Workspace(t, "Branch choices", "branch-"+uuid.NewString(), testutil.Cols{"issue_prefix": "BC"})
	f := testutil.New(testPool, ws, testUserID)
	other := f.User(t, "Branch tester", uuid.NewString()+"@example.test")
	f.Member(t, ws, testUserID, "owner")
	f.Member(t, ws, other, "member")
	runtimeID := f.Runtime(t, "Branch runtime")
	agent := f.Agent(t, "Bug agent", runtimeID)
	otherAgent := f.Agent(t, "Other agent", runtimeID, testutil.Cols{"owner_id": other})
	// The test creates real confirmation issues, comments and native wakeups.
	// Clean their rows in this test's private workspace, including scheduler outputs.
	for _, table := range []string{"issue", "inbox_item", "issue_wakeup", "comment", "activity_log"} {
		f.Cleanup(t, "DELETE FROM "+table+" WHERE workspace_id=$1", ws)
	}
	f.Cleanup(t, "DELETE FROM issue_subscriber WHERE issue_id IN (SELECT id FROM issue WHERE workspace_id=$1)", ws)
	f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE workspace_id=$1)", ws)
	f.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE workspace_id=$1)", ws)
	bug := func(name, assignee string) string {
		meta, _ := json.Marshal(map[string]any{"bug_assignee_name": name, "sync_source_env": "prod", "bug_version_id": 42, "external_source": "syndra", "multica_bug_automation": true})
		return f.Issue(t, "Imported", testutil.Cols{"origin_type": "external_issue", "origin_id": uuid.NewString(), "metadata": meta, "assignee_type": "agent", "assignee_id": assignee})
	}
	a, b, c := bug(handlerTestName, agent), bug(handlerTestName, agent), bug("Branch tester", otherAgent)
	f.Exec(t, "UPDATE workspace SET issue_counter=(SELECT max(number) FROM issue WHERE workspace_id=$1) WHERE id=$1", ws)
	repo := "https://github.com/example/repo.git"
	type answer struct {
		State, Branch, Marker string
		Ticket                string `json:"confirmation_issue_id"`
		Wake                  string `json:"wakeup_id"`
		Reply                 string `json:"decision_comment_id"`
	}
	request := func(user, issue, task, missing string) *http.Request {
		req := withURLParam(newRequestAs(user, "POST", "/", map[string]any{"repo": repo, "candidates": []string{"v3_wn"}, "missing_decision": missing}), "id", issue)
		req.Header.Set("X-Workspace-ID", ws)
		if task != "" {
			actor := agent
			if user == other {
				actor = otherAgent
			}
			testutil.WithHeaders(req, "X-Actor-Source", "task_token", "X-Agent-ID", actor, "X-Task-ID", task)
		}
		return req
	}
	read := func(user, issue, task, missing string) answer {
		t.Helper()
		var ans answer
		testutil.Call(t, testHandler.ResolveSyndraBranch, request(user, issue, task, missing)).Want(200).JSON(&ans)
		return ans
	}
	comment := func(user, issue, msg, parent string) {
		t.Helper()
		body := map[string]any{"content": msg}
		if parent != "" {
			body["parent_id"] = parent
		}
		req := withURLParam(newRequestAs(user, "POST", "/", body), "id", issue)
		req.Header.Set("X-Workspace-ID", ws)
		testutil.Call(t, testHandler.CreateComment, req).Want(201)
	}
	start := func(issue string) string {
		return f.Task(t, agent, testutil.Cols{"runtime_id": runtimeID, "issue_id": issue, "originator_user_id": testUserID, "accountable_user_id": testUserID, "status": "running"})
	}
	ta, tb := start(a), start(b)
	var wg sync.WaitGroup
	responses := make([]*httptest.ResponseRecorder, 2)
	for j, req := range []*http.Request{request(testUserID, a, ta, ""), request(testUserID, b, tb, "")} {
		responses[j] = httptest.NewRecorder()
		wg.Add(1)
		go func() { defer wg.Done(); testHandler.ResolveSyndraBranch(responses[j], req) }()
	}
	wg.Wait()
	var pending [2]answer
	for j, r := range responses {
		if r.Code != 200 {
			t.Fatalf("concurrent lookup %d: %d %s", j, r.Code, r.Body.String())
		}
		if err := json.Unmarshal(r.Body.Bytes(), &pending[j]); err != nil {
			t.Fatal(err)
		}
	}
	first, second := pending[0], pending[1]
	if first.State != "waiting" || first.Ticket == "" || first.Ticket != second.Ticket || first.Wake == "" || second.Wake == "" || first.Wake == second.Wake {
		t.Fatalf("waiting scopes: %+v", pending)
	}
	if again := read(testUserID, a, ta, ""); again.Wake != first.Wake {
		t.Fatalf("repeated lookup created a second wakeup: %+v", again)
	}
	tc := f.Task(t, otherAgent, testutil.Cols{"runtime_id": runtimeID, "issue_id": c, "originator_user_id": other, "accountable_user_id": other, "status": "running"})
	separate := read(other, c, tc, "")
	if separate.Wake == "" || separate.Ticket == first.Ticket {
		t.Fatal("different members shared a ticket")
	}
	comment(other, first.Ticket, "默认 wrong", "")
	if v := read(testUserID, a, ta, ""); v.State != "waiting" {
		t.Fatal("another member chose the default")
	}
	comment(testUserID, first.Ticket, "确认", "")
	got := read(testUserID, b, tb, "")
	if got.State != "personal_default" || got.Branch != "v3_wn" || got.Reply == "" {
		t.Fatalf("did not reuse first bug's default: %+v", got)
	}
	if v := read(other, c, "", ""); v.State != "waiting" {
		t.Fatal("other member inherited the default")
	}
	// Let both source runs finish and drive the real condition scheduler.
	f.Exec(t, "UPDATE issue SET status='blocked' WHERE id IN ($1,$2,$3) AND workspace_id=$4", a, b, c, ws)
	f.Exec(t, "UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id IN ($1,$2,$3)", ta, tb, tc)
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE workspace_id=$1", ws)
	svc := service.IssueWakeupService{Tasks: testHandler.TaskService}
	if err := svc.TickWorkspaces(t.Context(), parseUUID(ws)); err != nil {
		t.Fatal(err)
	}
	for _, issue := range []string{a, b} {
		if n := f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status='queued'", issue); n != 1 {
			t.Fatalf("issue %s: queued %d successor runs", issue, n)
		}
	}
	if n := f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status='queued'", c); n != 0 {
		t.Fatal("different member was woken")
	}
	prompt := f.Comment(t, a, "Branch scope\n"+first.Marker, testutil.Cols{"author_type": "agent", "author_id": agent})
	comment(testUserID, a, "仅本单 patch-wn", prompt)
	comment(testUserID, first.Ticket, "更换默认 release_wn", "")
	if v := read(testUserID, a, "", ""); v.State != "issue_override" || v.Branch != "patch-wn" {
		t.Fatalf("override lost: %+v", v)
	}
	if v := read(testUserID, b, "", ""); v.Branch != "release_wn" {
		t.Fatalf("default replacement lost: %+v", v)
	}
	if v := read(testUserID, b, "", got.Reply); v.Branch != "release_wn" {
		t.Fatal("obsolete missing-branch report reopened new decision")
	}
	current := read(testUserID, b, "", "")
	if v := read(testUserID, b, "", current.Reply); v.State != "waiting" || v.Ticket != first.Ticket {
		t.Fatalf("deleted branch not reopened: %+v", v)
	}
	comment(other, separate.Ticket, "默认 v3_other", "")
	f.Exec(t, `UPDATE issue SET metadata=jsonb_set(metadata,'{bug_assignee_name}',to_jsonb($2::text)) WHERE id=$1 AND workspace_id=$3`, a, "Branch tester", ws)
	if v := read(other, a, "", ""); v.Branch != "v3_other" || v.State != "personal_default" {
		t.Fatalf("previous member's override survived reassignment: %+v", v)
	}
	testutil.Call(t, testHandler.ResolveSyndraBranch, request(testUserID, a, "", "")).Want(403)
}
