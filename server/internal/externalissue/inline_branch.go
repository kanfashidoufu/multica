package externalissue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
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

// A short name is only a selector. A write still elects one authenticated
// scope across all of the member's eligible waits before saving the receipt.
func branchRepoMatches(selector, repo string) bool {
	return selector == "" || selector == repo || selector == path.Base(repo)
}

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
	body := fmt.Sprintf("我在 %s 等你选修复分支。私聊「黑默丁格·Bug 修复专家」说用哪条就好，也可以在原 Bug 评论「%s %s 分支名」。", scope.Repo, verb, scope.Repo)
	if len(candidates) > 0 {
		body += "\n候选：\n" + strings.Join(candidates, "\n")
	}
	// Also keep a complete, copyable shortcut for clients without a chat run.
	if len(candidates) == 1 {
		command = strings.Replace(command, "<分支名>", candidates[0], 1)
	}
	body += "\n快捷回复：" + command
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
	ticket, changed, err := i.applyBranchChoice(ctx, bug, c, nil)
	if errors.Is(err, errInlineNotAvailable) {
		return db.Issue{}, false, nil
	}
	return ticket, changed, err
}

var errInlineNotAvailable = errors.New("inline branch confirmation unavailable")

type BranchOption struct {
	Repo       string   `json:"repo"`
	Candidates []string `json:"candidates"`
}
type branchWait struct {
	BranchOption
	ticket  db.Issue
	scope   BranchScope
	live    bool
	restore *db.ListIssueWakeupsRow
}

func branchCandidates(ticket db.Issue) []string {
	_, list, ok := strings.Cut(ticket.Description.String, "\n候选分支：\n")
	if !ok {
		return nil
	}
	var refs []string
	for _, line := range strings.Split(list, "\n") {
		ref, ok := strings.CutPrefix(line, "- ")
		if !ok || !ValidBranch(ref) {
			break
		}
		refs = append(refs, ref)
	}
	return refs
}

// Reads and writes share every scope, identity and wait-liveness check.
func (i *Importer) branchWaits(ctx context.Context, bug db.Issue, user pgtype.UUID, includeSatisfied bool) ([]*branchWait, error) {
	if !IsSyndraBug(bug) || !bugAutomationEnrolled(bug.Metadata) {
		return nil, nil
	}
	q := i.Queries
	ws, err := q.GetWorkspace(ctx, bug.WorkspaceID)
	if err == nil && ws.Slug != ExpertWorkspace {
		return nil, errInlineNotAvailable
	}
	if err != nil {
		return nil, err
	}
	bug, err = q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: bug.ID, WorkspaceID: bug.WorkspaceID})
	if err != nil {
		return nil, err
	}
	if bug.Status == "done" || bug.Status == "cancelled" || bug.AssigneeType.String != "agent" {
		return nil, fmt.Errorf("%w: 当前 Bug 没有待确认的修复任务", ErrBranchInput)
	}
	member, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{WorkspaceID: ws.ID, UserID: user})
	if err != nil {
		return nil, err
	}
	visible, err := projectaccess.ForMember(ctx, q, member)
	if err != nil {
		return nil, err
	}
	if !visible.CanSeeProject(bug.ProjectID) {
		return nil, ErrBranchInput
	}
	waits, err := q.ListIssueWakeups(ctx, db.ListIssueWakeupsParams{WorkspaceID: ws.ID, IssueID: bug.ID, AgentIds: []pgtype.UUID{bug.AssigneeID}})
	if err != nil {
		return nil, err
	}
	targets := map[string]*branchWait{}
	var scopeFailure error
	for _, w := range waits {
		if w.DisabledAt.Valid || w.TimedOutAt.Valid || w.PausedReason.Valid || w.ExpiresAt.Valid && !w.ExpiresAt.Time.After(time.Now()) || w.AgentID != bug.AssigneeID || w.CreatedBy != user || !strings.HasPrefix(w.Instruction, BranchWaitInstruction) {
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
			return nil, err
		}
		s, ok := choiceTicket(ticket)
		if !ok || !visible.CanSeeProject(ticket.ProjectID) || s.Member != util.UUIDToString(user) || w.Instruction != BranchWaitInstruction+s.Marker() {
			continue
		}
		if !w.Enabled {
			var m map[string]json.RawMessage
			if ticket.Status != "done" || !w.LastTaskID.Valid || !w.SourceTaskID.Valid || w.Kind != "event" || w.Mode != "once" || json.Unmarshal(ticket.Metadata, &m) != nil || m[BranchDecisionKey] != nil {
				continue
			}
		} else if !includeSatisfied && ticket.Status == "done" {
			// An already satisfied repository is not another pending choice.
			var m map[string]json.RawMessage
			if json.Unmarshal(ticket.Metadata, &m) == nil && m[BranchDecisionKey] != nil {
				continue
			}
		}
		current, err := i.BranchScope(ctx, bug, "https://"+s.Repo)
		if errors.Is(err, ErrBranchInput) || err == nil && current != s {
			scopeFailure = fmt.Errorf("%w: 这条等待和 Bug 现在的负责人、版本或试用资格不再匹配，需要专家重新核对。", ErrBranchInput)
			continue
		}
		if err != nil {
			return nil, err
		}
		k := s.Repo
		if targets[k] == nil {
			targets[k] = &branchWait{BranchOption: BranchOption{s.Repo, branchCandidates(ticket)}, ticket: ticket, scope: s}
		}
		if w.Enabled {
			targets[k].live = true
		} else {
			targets[k].restore = &w
		}
	}
	if len(targets) == 0 && scopeFailure != nil {
		return nil, scopeFailure
	}
	out := make([]*branchWait, 0, len(targets))
	for _, item := range targets {
		out = append(out, item)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Repo < out[b].Repo })
	return out, nil
}

func (i *Importer) PendingBranchOptions(ctx context.Context, bug db.Issue, user pgtype.UUID) ([]BranchOption, error) {
	targets, err := i.branchWaits(ctx, bug, user, false)
	if err != nil {
		return nil, err
	}
	out := make([]BranchOption, 0, len(targets))
	for _, v := range targets {
		out = append(out, v.BranchOption)
	}
	return out, nil
}

func (i *Importer) ApplyFeishuBranchReply(ctx context.Context, bug db.Issue, c db.Comment, evidence *FeishuBranchEvidence) (db.Issue, bool, error) {
	if evidence == nil {
		evidence = &FeishuBranchEvidence{}
	}
	return i.applyBranchChoice(ctx, bug, c, evidence)
}
func (i *Importer) applyBranchChoice(ctx context.Context, bug db.Issue, c db.Comment, evidence *FeishuBranchEvidence) (db.Issue, bool, error) {
	if c.AuthorType != "member" || c.IssueID != bug.ID || c.WorkspaceID != bug.WorkspaceID {
		return db.Issue{}, false, ErrBranchInput
	}
	kind, repo, branch := inlineChoice(c.Content)
	waits, err := i.branchWaits(ctx, bug, c.AuthorID, repo != "")
	if err != nil {
		return db.Issue{}, false, err
	}
	if kind == "" {
		return db.Issue{}, false, fmt.Errorf("%w: 分支名没有读完整，可以把完整名字发我吗？", ErrBranchInput)
	}
	var chosen *branchWait
	var known []string
	for _, w := range waits {
		known = append(known, w.Repo)
		if !branchRepoMatches(repo, w.Repo) {
			continue
		}
		if chosen != nil {
			return db.Issue{}, false, fmt.Errorf("%w: 有多个仓库在等，你指的是 %s 还是 %s？", ErrBranchInput, chosen.Repo, w.Repo)
		}
		chosen = w
	}
	if chosen == nil {
		if len(known) > 0 {
			return db.Issue{}, false, fmt.Errorf("%w: 这单正在等 %s 的分支，你想选的是哪个？", ErrBranchInput, strings.Join(known, "、"))
		}
		return db.Issue{}, false, fmt.Errorf("%w: 目前没找到这个账号在这单上待确认的分支，我不能替其他负责人选择。", ErrBranchInput)
	}
	q := i.Queries
	target, targetScope, live, restore := chosen.ticket, chosen.scope, chosen.live, chosen.restore
	locked, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: target.ID, WorkspaceID: bug.WorkspaceID})
	if err != nil {
		return db.Issue{}, false, err
	}
	if s, ok := choiceTicket(locked); !ok || s != targetScope || locked.Status == "cancelled" {
		return db.Issue{}, false, ErrBranchInput
	}
	if evidence != nil {
		candidates := branchCandidates(locked)
		if evidence.SourceTaskID.Valid && !evidence.supports(branch, candidates) || !evidence.SourceTaskID.Valid && len(candidates) > 0 && !slices.Contains(candidates, branch) {
			return db.Issue{}, false, fmt.Errorf("%w: 还不能把这条消息唯一对应到这个分支。请和本人核对具体候选，不要报告已确认。", ErrBranchInput)
		}
	}
	updated, changed, err := saveBranchReceipt(ctx, q, locked, c, kind, branch)
	if err != nil || !changed || live || restore == nil {
		return updated, changed, err
	}
	// A manual "done" on an old ticket could consume its condition before any
	// receipt existed. Give the verified reply one fresh native condition; the
	// scheduler rechecks invocation/project access and coalesces waiting runs.
	_, err = q.CreateIssueWakeup(ctx, db.CreateIssueWakeupParams{ID: dbid.NewV7(), WorkspaceID: bug.WorkspaceID, IssueID: bug.ID, AgentID: bug.AssigneeID, CreatedBy: c.AuthorID, Instruction: restore.Instruction,
		Kind: "event", Mode: "once", EventTypes: restore.EventTypes, Condition: restore.Condition, Timezone: restore.Timezone, NextFireAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}})
	return updated, changed, err
}
