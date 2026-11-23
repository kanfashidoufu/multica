package externalissue

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestExternalSyncReturnsPreservedTriageState(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t, ctx)
	q := db.New(pool)
	fx := createImporterFixture(t, ctx, pool, q)
	dbfx := testutil.New(pool, util.UUIDToString(fx.Workspace.ID), util.UUIDToString(fx.User.ID))

	for _, state := range []pgtype.Text{{}, {String: "pending", Valid: true}} {
		name := state.String
		if !state.Valid {
			name = "ordinary"
		}
		t.Run(name, func(t *testing.T) {
			id := dbfx.Issue(t, "Before sync", testutil.Cols{
				"description_revision": 10,
				"triage_state":         state,
				"origin_type":          "external_issue",
				"assignee_type":        "member",
				"assignee_id":          fx.User.ID,
			})
			issueID, err := util.ParseUUID(id)
			if err != nil {
				t.Fatal(err)
			}
			params := db.UpdateIssueFromExternalSyncParams{
				ID: issueID, WorkspaceID: fx.Workspace.ID,
				Title: "After sync", Description: util.StrToText("Synced description"),
				Status: "todo", Priority: "high", Metadata: []byte("{}"),
			}
			updated, err := q.UpdateIssueFromExternalSync(ctx, params)
			if err != nil {
				t.Fatalf("UpdateIssueFromExternalSync: %v", err)
			}
			if updated.DescriptionRevision != 11 {
				t.Fatalf("changed description revision = %d, want 11", updated.DescriptionRevision)
			}
			params.Title = "Title-only sync"
			replayed, err := q.UpdateIssueFromExternalSync(ctx, params)
			if err != nil || replayed.DescriptionRevision != updated.DescriptionRevision {
				t.Fatalf("title-only sync revision = %d, want %d (err %v)", replayed.DescriptionRevision, updated.DescriptionRevision, err)
			}
			stored, err := q.GetIssue(ctx, issueID)
			if err != nil {
				t.Fatalf("GetIssue: %v", err)
			}
			if updated.TriageState != state || stored.TriageState != state {
				t.Fatalf("triage state: response=%+v stored=%+v, want %+v", updated.TriageState, stored.TriageState, state)
			}
			if updated.Title != "After sync" || updated.AssigneeID != fx.User.ID || updated.OriginType.String != "external_issue" {
				t.Fatalf("sync must update mirrored content and preserve local ownership/origin: %+v", updated)
			}
		})
	}
}
