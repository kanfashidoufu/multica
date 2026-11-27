package externalissue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/projectaccess"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

const ExpertWorkspace = "qtb-fe"

func (i *Importer) NotifyBranchWait(ctx context.Context, bug db.Issue, scope BranchScope, wakeID pgtype.UUID, command string, candidates []string) error {
	// A repeated resolve must not re-prompt; the wakeup and the inbox item
	// have the same lifetime key even if the first delivery attempt failed.
	_, err := i.Queries.GetInboxItemInWorkspace(ctx, db.GetInboxItemInWorkspaceParams{WorkspaceID: bug.WorkspaceID, ID: wakeID})
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	memberID, err := util.ParseUUID(scope.Member)
	if err != nil {
		return err
	}
	member, err := i.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{WorkspaceID: bug.WorkspaceID, UserID: memberID})
	if err != nil {
		return err
	}
	visible, err := projectaccess.ForMember(ctx, i.Queries, member)
	if err != nil || !visible.CanSeeProject(bug.ProjectID) {
		return err
	}
	verb := "默认"
	if strings.HasPrefix(command, "更换分支 ") {
		verb = "更换默认"
	}
	body := fmt.Sprintf("请确认修复分支。仓库：%s\n在本 Bug 评论「%s %s 分支名」，或私聊黑默丁格·Bug 修复专家：\n%s", scope.Repo, verb, scope.Repo, command)
	if len(candidates) > 0 {
		body += "\n候选：\n" + strings.Join(candidates, "\n")
	}
	item, err := i.Queries.CreateInboxItem(ctx, db.CreateInboxItemParams{ID: wakeID, WorkspaceID: bug.WorkspaceID, IssueID: bug.ID, RecipientType: "member", RecipientID: memberID, Type: "agent_blocked", Severity: "action_required", Title: bug.Title, Body: util.StrToText(body), Details: []byte(`{"reason":"syndra_branch_confirmation"}`)})
	if isUniqueViolation(err, "inbox_item_pkey") {
		return nil
	}
	if err != nil {
		return err
	}
	i.publishInboxNew(item, bug.Status)
	return nil
}

// The immutable import origin, not an editable title, label or metadata flag,
// distinguishes actual Syndra bugs from other external issues and branch anchors.
func IsSyndraBug(bug db.Issue) bool {
	var m struct {
		Source string `json:"external_source"`
		Env    string `json:"sync_source_env"`
		Entity string `json:"bug_entity_type"`
		Key    string `json:"external_key"`
	}
	return bug.OriginType.String == OriginType && json.Unmarshal(bug.Metadata, &m) == nil &&
		m.Source == defaultBugProvider && m.Env != "" && m.Key != "" &&
		bug.OriginID == sourceOriginID(m.Source, m.Env, firstNonEmpty(m.Entity, "version_bug"), m.Key)
}

// Optional middle argument is the canonical host/org/repo shown in the prompt.
func inlineChoice(content string) (kind, repo, branch string) {
	p := strings.Fields(content)
	if len(p) != 2 && len(p) != 3 || p[0] != "默认" && p[0] != "更换默认" {
		return "", "", ""
	}
	kind, branch = branchReply(p[0] + " " + p[len(p)-1])
	if len(p) == 3 {
		repo = p[1]
	}
	return
}

func IsInlineBranchCommand(content string) bool {
	p := strings.Fields(content)
	return len(p) > 0 && (p[0] == "默认" || p[0] == "更换默认")
}

// Older clients could close a ticket after the editor escaped its underscores.
// Only recover an unambiguous choice personally posted on that exact ticket.
func (i *Importer) RecoverClosedBranchTicket(ctx context.Context, ticket db.Issue, scope BranchScope) (db.Issue, error) {
	tx, err := i.IssueService.TxStarter.Begin(ctx)
	if err != nil {
		return ticket, err
	}
	defer tx.Rollback(ctx)
	q := i.Queries.WithTx(tx)
	ticket, err = q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: ticket.ID, WorkspaceID: ticket.WorkspaceID})
	if err != nil {
		return ticket, err
	}
	branch, _, err := ConfirmedBranch(ctx, q, ticket, scope)
	if err != nil || branch != "" || ticket.Status != "done" {
		return ticket, err
	}
	rows, err := q.ListCommentsForIssue(ctx, db.ListCommentsForIssueParams{IssueID: ticket.ID, WorkspaceID: ticket.WorkspaceID, Limit: 101})
	if err != nil || len(rows) > 100 {
		return ticket, err
	}
	var chosen db.Comment
	for _, c := range rows {
		if c.AuthorType != "member" || util.UUIDToString(c.AuthorID) != scope.Member || c.DeletedAt.Valid {
			continue
		}
		kind, value := ticketReply(ticket, c.Content)
		if kind != "默认" && kind != "更换默认" {
			continue
		}
		if branch != "" && branch != value {
			return ticket, nil
		}
		chosen, branch = c, value
	}
	if !chosen.ID.Valid {
		return ticket, nil
	}
	ticket, _, err = ApplyBranchReply(ctx, q, ticket, chosen)
	if err != nil {
		return ticket, err
	}
	return ticket, tx.Commit(ctx)
}

// ApplyInlineBranchReply runs in the transaction that inserted the original
// human comment (which locks the bug). A live native wait binds the reply to a
// real repository discovery. Never select a repository by candidate ordering.
func (i *Importer) ApplyInlineBranchReply(ctx context.Context, bug db.Issue, c db.Comment) (db.Issue, bool, error) {
	if c.AuthorType != "member" || c.IssueID != bug.ID || c.WorkspaceID != bug.WorkspaceID || !IsInlineBranchCommand(c.Content) || !IsSyndraBug(bug) || !bugAutomationEnrolled(bug.Metadata) {
		return db.Issue{}, false, nil
	}
	q := i.Queries
	ws, err := q.GetWorkspace(ctx, bug.WorkspaceID)
	if err != nil || ws.Slug != ExpertWorkspace {
		return db.Issue{}, false, err
	}
	bug, err = q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: bug.ID, WorkspaceID: bug.WorkspaceID})
	if err != nil {
		return db.Issue{}, false, err
	}
	kind, repo, branch := inlineChoice(c.Content)
	if kind == "" {
		return db.Issue{}, false, fmt.Errorf("%w: 使用「默认 [仓库] 分支名」", ErrBranchInput)
	}
	if bug.Status == "done" || bug.Status == "cancelled" || bug.AssigneeType.String != "agent" {
		return db.Issue{}, false, fmt.Errorf("%w: 当前 Bug 没有待确认的修复任务", ErrBranchInput)
	}
	member, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{WorkspaceID: ws.ID, UserID: c.AuthorID})
	if err != nil {
		return db.Issue{}, false, err
	}
	visible, err := projectaccess.ForMember(ctx, q, member)
	if err != nil {
		return db.Issue{}, false, err
	}
	if !visible.CanSeeProject(bug.ProjectID) {
		return db.Issue{}, false, ErrBranchInput
	}
	waits, err := q.ListIssueWakeups(ctx, db.ListIssueWakeupsParams{WorkspaceID: ws.ID, IssueID: bug.ID, AgentIds: []pgtype.UUID{bug.AssigneeID}})
	if err != nil {
		return db.Issue{}, false, err
	}
	var target db.Issue
	var targetScope BranchScope
	var restore *db.ListIssueWakeupsRow
	var live bool
	for _, w := range waits {
		if w.DisabledAt.Valid || w.TimedOutAt.Valid || w.PausedReason.Valid || w.ExpiresAt.Valid && !w.ExpiresAt.Time.After(time.Now()) || w.AgentID != bug.AssigneeID || w.CreatedBy != c.AuthorID || !strings.HasPrefix(w.Instruction, BranchWaitInstruction) {
			continue
		}
		var cond service.WakeupCondition
		if json.Unmarshal(w.Condition, &cond) != nil || cond.Type != "other_issue" || cond.State != "done" {
			continue
		}
		id, err := util.ParseUUID(cond.IssueID)
		if err != nil {
			continue
		}
		ticket, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: id, WorkspaceID: ws.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return db.Issue{}, false, err
		}
		s, ok := choiceTicket(ticket)
		if !ok || !visible.CanSeeProject(ticket.ProjectID) || s.Member != util.UUIDToString(c.AuthorID) || (repo != "" && repo != s.Repo) || w.Instruction != BranchWaitInstruction+s.Marker() {
			continue
		}
		if !w.Enabled {
			var m map[string]json.RawMessage
			if ticket.Status != "done" || !w.LastTaskID.Valid || !w.SourceTaskID.Valid || w.Kind != "event" || w.Mode != "once" || json.Unmarshal(ticket.Metadata, &m) != nil || m[BranchDecisionKey] != nil {
				continue
			}
		} else if repo == "" && ticket.Status == "done" {
			// An already satisfied repository is not another pending choice.
			var m map[string]json.RawMessage
			if json.Unmarshal(ticket.Metadata, &m) == nil && m[BranchDecisionKey] != nil {
				continue
			}
		}
		current, err := i.BranchScope(ctx, bug, "https://"+s.Repo)
		if errors.Is(err, ErrBranchInput) || err == nil && current != s {
			continue
		}
		if err != nil {
			return db.Issue{}, false, err
		}
		if target.ID.Valid && target.ID != ticket.ID {
			return db.Issue{}, false, fmt.Errorf("%w: 多个仓库正在等待，请使用「默认 仓库 分支名」", ErrBranchInput)
		}
		target, targetScope = ticket, s
		if w.Enabled {
			live = true
		} else {
			restore = &w
		}
	}
	if !target.ID.Valid {
		return db.Issue{}, false, fmt.Errorf("%w: 没有属于你和该仓库的分支等待，请核对原 Bug 和当前 Syndra 负责人", ErrBranchInput)
	}
	locked, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: target.ID, WorkspaceID: ws.ID})
	if err != nil {
		return db.Issue{}, false, err
	}
	if s, ok := choiceTicket(locked); !ok || s != targetScope || locked.Status == "cancelled" {
		return db.Issue{}, false, ErrBranchInput
	}
	updated, changed, err := saveBranchReceipt(ctx, q, locked, c, kind, branch)
	if err != nil || !changed || live || restore == nil {
		return updated, changed, err
	}
	// A manual "done" on an old ticket could consume its condition before any
	// receipt existed. Give the verified reply one fresh native condition; the
	// scheduler rechecks invocation/project access and coalesces waiting runs.
	_, err = q.CreateIssueWakeup(ctx, db.CreateIssueWakeupParams{ID: dbid.NewV7(), WorkspaceID: ws.ID, IssueID: bug.ID, AgentID: bug.AssigneeID, CreatedBy: c.AuthorID, Instruction: restore.Instruction,
		Kind: "event", Mode: "once", EventTypes: restore.EventTypes, Condition: restore.Condition, Timezone: restore.Timezone, NextFireAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}})
	return updated, changed, err
}
