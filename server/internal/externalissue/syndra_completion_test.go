package externalissue

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestSyndraCompletionRequiresOriginalRecord(t *testing.T) {
	key := "syndra:test:version_bug:42"
	row := db.LocalSyndraBugCompletion{OriginID: sourceOriginID("syndra", "test", "version_bug", key), SourceEnv: "test", BugID: "42", RecordKey: key, ReopenCount: "0"}
	if _, ok := completionPayload(row); !ok {
		t.Fatal("valid record rejected")
	}
	row.BugID = "99"
	row.RecordKey = "syndra:test:version_bug:99"
	if _, ok := completionPayload(row); ok {
		t.Fatal("editable metadata redirected completion to another bug")
	}
}

func TestSyndraCompletionOutbox(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable PostgreSQL server required")
	}
	pool := testutil.IsolatedDB(t, dsn)
	ctx := t.Context()
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	f := testutil.New(pool, util.UUIDToString(fx.Workspace.ID), util.UUIDToString(fx.User.ID))
	key := "syndra:test:version_bug:42"
	origin := sourceOriginID("syndra", "test", "version_bug", key)
	metadata := `{"external_source":"syndra","bug_entity_type":"version_bug","sync_source_env":"test","bug_id":42,"external_key":"syndra:test:version_bug:42"}`
	issueID := f.Issue(t, "reverse", testutil.Cols{"origin_type": OriginType, "origin_id": origin, "metadata": metadata})
	id, err := util.ParseUUID(issueID)
	if err != nil {
		t.Fatal(err)
	}
	count := func(want int) {
		t.Helper()
		if n := f.Count(t, "SELECT count(*) FROM local_syndra_bug_completion WHERE issue_id=$1", issueID); n != want {
			t.Fatalf("queued=%d want=%d", n, want)
		}
	}
	set := func(status string) {
		t.Helper()
		f.Exec(t, "UPDATE issue SET status=$2,revision=revision+1 WHERE id=$1", issueID, status)
	}
	result := func(want string) {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx, "SELECT outcome FROM local_syndra_bug_completion WHERE issue_id=$1", issueID).Scan(&got); err != nil || got != want {
			t.Fatalf("outcome=%q err=%v", got, err)
		}
	}
	set("in_review")
	count(0)
	_, err = q.UpdateIssueFromExternalSync(ctx, db.UpdateIssueFromExternalSyncParams{ID: id, WorkspaceID: fx.Workspace.ID, Title: "source", Status: "done", Priority: "none", Metadata: []byte(metadata)})
	if err != nil {
		t.Fatal(err)
	}
	count(0)
	set("todo")
	set("cancelled")
	count(0)
	set("done")
	count(1)
	result("Multica 已确认完成，未填写处理详情。")
	set("todo")
	count(0)
	agent := f.Agent(t, "resolver", f.Runtime(t, "runtime"))
	f.Task(t, agent, testutil.Cols{"issue_id": issueID, "status": "completed", "completed_at": testutil.Raw("now()"), "result": `{"comment":"最终处理说明"}`})
	set("done")
	result("最终处理说明")
	set("in_review")
	f.Comment(t, issueID, "处理结果：修复了筛选\n验证通过\n\n内部交接，不回写")
	f.Comment(t, issueID, "unrelated discussion")
	f.Insert(t, "issue_status", testutil.Cols{"workspace_id": f.WorkspaceID, "key": "qa_finished", "category": "done", "name": "验证完成", "color": "#001122"})
	set("qa_finished")
	count(1)
	result("修复了筛选\n验证通过")
	set("done")
	count(1)
	first := true
	var sent completionRequest
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://test-syndra.lggj.work/api/open/multica/version-bugs/resolve" {
			t.Fatalf("wrong destination: %s", req.URL)
		}
		b, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Authorization") != "Bearer "+completionSignature("secret", b) {
			t.Fatal("invalid signature")
		}
		var next completionRequest
		if err := json.Unmarshal(b, &next); err != nil {
			t.Fatal(err)
		}
		if first {
			first = false
			sent = next
			return nil, errors.New("lost response")
		}
		if next != sent || next.Outcome != "修复了筛选\n验证通过" || next.BugID != 42 {
			t.Fatalf("changed retry snapshot: %#v", next)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true,"code":4000,"data":{"status":"duplicate","event_id":"` + next.EventID + `"}}`))}, nil
	})}
	if _, err := deliverNextCompletion(ctx, q, fx.Workspace.ID, client, "secret"); err == nil {
		t.Fatal("lost response counted as delivery")
	}
	count(1)
	if _, err := q.ClaimSyndraCompletion(ctx, fx.Workspace.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("active lease was not respected")
	}
	f.Exec(t, "UPDATE local_syndra_bug_completion SET available_at=now() WHERE issue_id=$1", issueID)
	if found, err := deliverNextCompletion(ctx, q, fx.Workspace.ID, client, "secret"); !found || err != nil {
		t.Fatalf("retry: %v", err)
	}
	count(0)
}
