package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/externalissue"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func branchError(w http.ResponseWriter, err error) {
	if errors.Is(err, externalissue.ErrBranchInput) {
		writeError(w, 409, err.Error())
		return
	}
	writeError(w, 500, "could not resolve personal branch decision")
}

// The receipt is written together with its human comment. Public metadata
// edits must not replay a superseded reply or erase it to change the default.
func ProtectSyndraBranchReceipt(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if chi.URLParam(r, "key") == externalissue.BranchDecisionKey {
			writeError(w, 403, "use a member reply on the personal branch confirmation issue")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) syndraBranchImporter(q *db.Queries) externalissue.Importer {
	return externalissue.Importer{Queries: q, IssueService: h.IssueService, Bus: h.Bus, Config: externalissue.Config{BugAutomationAssignees: os.Getenv("MULTICA_EXTERNAL_BUG_AUTOMATION_ASSIGNEES")}}
}

func (h *Handler) ResolveSyndraBranch(w http.ResponseWriter, r *http.Request) {
	bug, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var in struct {
		Repo            string
		Candidates      []string
		MissingDecision string `json:"missing_decision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&in); err != nil {
		writeError(w, 400, "invalid branch request")
		return
	}
	for _, c := range in.Candidates {
		if !externalissue.ValidBranch(c) {
			writeError(w, 400, "invalid candidate branch")
			return
		}
	}
	ctx := r.Context()
	i := h.syndraBranchImporter(h.Queries)
	scope, err := i.BranchScope(ctx, bug, in.Repo)
	if err != nil {
		branchError(w, err)
		return
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), scope.Workspace)
	// The shared executor acts for the current source member. Reassignment
	// must not let a run started for the previous member use the new default.
	if actorType == "agent" {
		task, valid := h.taskFromRequestHeader(r)
		if !valid || task.IssueID != bug.ID || task.AgentID != bug.AssigneeID || uuidToString(task.AgentID) != actorID || bug.AssigneeType.String != "agent" {
			writeError(w, 403, "not the assigned bug task")
			return
		}
		agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: bug.AssigneeID, WorkspaceID: bug.WorkspaceID})
		if err != nil {
			branchError(w, err)
			return
		}
		shared, err := externalissue.IsWorkspaceBugExpert(ctx, h.Queries, agent, bug.WorkspaceID)
		if err != nil {
			branchError(w, err)
			return
		}
		if !shared {
			writeError(w, 403, "assigned agent is not the workspace bug repair expert")
			return
		}
		if isTerminalTaskStatus(task.Status) || uuidToString(task.OriginatorUserID) != scope.Member {
			writeError(w, 409, "Syndra assignee changed; preserve work and hand off to the current member")
			return
		}
	} else if actorID != scope.Member {
		writeError(w, 403, "not the current Syndra assignee")
		return
	}
	result := map[string]any{"scope": scope, "marker": scope.Marker()}
	// Only a member reply in this bug's current scope is an override. The
	// parent binds the compact human reply to the repo/version on that turn.
	var text, id string
	err = h.DB.QueryRow(ctx, `SELECT c.content,c.id::text FROM comment c JOIN comment p
		ON p.id=c.parent_id AND p.workspace_id=c.workspace_id AND p.issue_id=c.issue_id
		WHERE c.workspace_id=$1 AND c.issue_id=$2 AND c.author_type='member' AND c.author_id=$3
		AND c.deleted_at IS NULL AND p.deleted_at IS NULL AND p.author_type='agent'
		AND strpos(chr(10)||p.content||chr(10),chr(10)||$4||chr(10))>0
		AND p.content NOT LIKE '%[syndra-branch:%[syndra-branch:%' AND c.content LIKE '仅本单 %'
		ORDER BY c.created_at DESC,c.id DESC LIMIT 1`, bug.WorkspaceID, bug.ID, scope.Member, scope.Marker()).Scan(&text, &id)
	if err == nil {
		branch := strings.ReplaceAll(strings.TrimSpace(strings.TrimPrefix(text, "仅本单 ")), `\_`, "_")
		if !externalissue.ValidBranch(branch) {
			writeError(w, 409, "invalid per-issue branch; ask the current assignee to correct it")
			return
		}
		if err := i.EndBranchWait(ctx, bug, scope); err != nil {
			branchError(w, err)
			return
		}
		result["state"], result["branch"], result["decision_comment_id"] = "issue_override", branch, id
		writeJSON(w, 200, result)
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		branchError(w, err)
		return
	}
	ws, err := h.Queries.GetWorkspace(ctx, bug.WorkspaceID)
	if err != nil {
		branchError(w, err)
		return
	}
	inline := ws.Slug == externalissue.ExpertWorkspace && externalissue.IsSyndraBug(bug)
	ticket, err := i.BranchTicket(ctx, bug, scope, in.Candidates, actorType, parseUUID(actorID), inline)
	if err != nil {
		branchError(w, err)
		return
	}
	if !h.requireProjectVisible(w, r, scope.Workspace, ticket.ProjectID, 404, "confirmation issue not found") {
		return
	}
	result["confirmation_issue_id"] = uuidToString(ticket.ID)
	branch, receipt, err := externalissue.ConfirmedBranch(ctx, h.Queries, ticket, scope)
	if err != nil {
		branchError(w, err)
		return
	}
	if inline && ticket.Status == "done" && branch == "" {
		ticket, err = i.RecoverClosedBranchTicket(ctx, ticket, scope)
		if err != nil {
			branchError(w, err)
			return
		}
		branch, receipt, err = externalissue.ConfirmedBranch(ctx, h.Queries, ticket, scope)
		if err != nil {
			branchError(w, err)
			return
		}
	}
	if ticket.Status == "done" && ((in.MissingDecision != "" && in.MissingDecision == receipt) || (inline && branch == "")) {
		// Do not invalidate a replacement another member reply just committed.
		ticket, err = h.Queries.UpdateIssue(ctx, db.UpdateIssueParams{
			ID: ticket.ID, ExpectedRevision: pgtype.Int8{Int64: ticket.Revision, Valid: true}, Status: pgtype.Text{String: "todo", Valid: true},
			AssigneeType: ticket.AssigneeType, AssigneeID: ticket.AssigneeID, ParentIssueID: ticket.ParentIssueID,
			ProjectID: ticket.ProjectID, Stage: ticket.Stage, StartDate: ticket.StartDate, DueDate: ticket.DueDate,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 409, "decision changed; resolve again")
			return
		}
		if err != nil {
			branchError(w, err)
			return
		}
		h.publishIssueSnapshots(ctx, []db.Issue{ticket}, actorType, actorID)
		result["reopened"] = true
	}
	if ticket.Status == "done" && branch != "" {
		result["state"], result["branch"], result["decision_comment_id"] = "personal_default", branch, receipt
		writeJSON(w, 200, result)
		return
	}
	if ticket.Status == "done" || ticket.Status == "cancelled" {
		writeError(w, 409, "confirmation was closed without a valid branch; ask the assignee to reopen it")
		return
	}
	result["state"] = "waiting"
	var command string
	if inline {
		result["reply_on_bug"] = true
		verb := "确认分支"
		if branch != "" {
			verb = "更换分支"
		}
		command = verb + " " + service.IssueIdentifier(ws.IssuePrefix, bug.Number) + " " + scope.Repo + " <分支名>"
		result["feishu_command"] = command
	}
	if actorType == "agent" {
		originator := h.invokeOriginatorFromRequest(r, actorType, actorID)
		if originator == "" {
			writeError(w, 403, "a human originator is required to register a wakeup")
			return
		}
		c := service.WakeupCondition{Type: "other_issue", IssueID: uuidToString(ticket.ID), State: "done"}
		var existing string
		instruction := externalissue.BranchWaitInstruction + scope.Marker()
		err = h.DB.QueryRow(ctx, `SELECT id::text FROM issue_wakeup WHERE workspace_id=$1 AND issue_id=$2 AND agent_id=$3 AND enabled AND paused_reason IS NULL AND condition->>'type'='other_issue' AND condition->>'issue_id'=$4 AND condition->>'state'='done' AND instruction=$5 AND (expires_at IS NULL OR expires_at>now()) LIMIT 1`, bug.WorkspaceID, bug.ID, bug.AssigneeID, c.IssueID, instruction).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			condition, _ := json.Marshal(c)
			svc := service.IssueWakeupService{Tasks: h.TaskService}
			rule, err := svc.Create(ctx, bug.ID, parseUUID(originator), h.wakeupSourceTaskID(r), service.WakeupInput{Kind: "event", Mode: "once", AgentID: actorID, Condition: condition, Instruction: instruction})
			if err != nil {
				wakeupError(w, err)
				return
			}
			existing = uuidToString(rule.ID)
		} else if err != nil {
			branchError(w, err)
			return
		}
		result["wakeup_id"] = existing
		if inline {
			if err := i.NotifyBranchWait(ctx, bug, scope, parseUUID(existing), command, in.Candidates); err != nil {
				branchError(w, err)
				return
			}
		}
	}
	writeJSON(w, 200, result)
}
