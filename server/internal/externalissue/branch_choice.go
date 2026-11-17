package externalissue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// A native issue supplies both durable human comments and a condition that
// other issues can wait on. Its unique external origin elects one question
// even when several agents on different daemons reach discovery together.
type BranchScope struct {
	Workspace string `json:"workspace"`
	Env       string `json:"env"`
	Repo      string `json:"repo"`
	Version   int64  `json:"version"`
	Member    string `json:"member"`
}

const scopePrefix = "<!-- syndra-branch-scope:"
const BranchDecisionKey = "syndra_branch_decision"
const BranchWaitInstruction = "Resume the Syndra bug after personal branch confirmation. Re-read its current assignee and resolve the branch again before editing or merging. "

var ErrBranchInput = errors.New("invalid Syndra branch context")

// Withdraw only this integration's waits. The importer holds the issue lock
// while changing its source scope; unrelated user-created wakeups survive.
func cancelBranchWaits(ctx context.Context, tx pgx.Tx, issue db.Issue, marker string) ([]db.AgentTaskQueue, error) {
	rows, err := tx.Query(ctx, `UPDATE issue_wakeup SET enabled=false,disabled_at=coalesce(disabled_at,now()),updated_at=now()
		WHERE workspace_id=$1 AND issue_id=$2 AND system_rule IS NULL AND condition->>'type'='other_issue'
		AND starts_with(instruction,$3) AND ($4='' OR instruction=$3||$4)
		AND (enabled OR last_task_id IS NOT NULL) RETURNING id`, issue.WorkspaceID, issue.ID, BranchWaitInstruction, marker)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
	if err != nil {
		return nil, err
	}
	var cancelled []db.AgentTaskQueue
	q := db.New(tx)
	for _, id := range ids {
		if err := q.DiscardWakeupReceipts(ctx, id); err != nil {
			return nil, err
		}
		tasks, err := q.CancelUnstartedWakeupTasks(ctx, util.UUIDToString(id))
		if err != nil {
			return nil, err
		}
		cancelled = append(cancelled, tasks...)
	}
	return cancelled, nil
}

func (i *Importer) EndBranchWait(ctx context.Context, issue db.Issue, scope BranchScope) error {
	tx, err := i.IssueService.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	cancelled, err := cancelBranchWaits(ctx, tx, issue, scope.Marker())
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	i.publishBranchWaitCancellation(ctx, issue, cancelled)
	return nil
}

func (i *Importer) publishBranchWaitCancellation(ctx context.Context, issue db.Issue, cancelled []db.AgentTaskQueue) {
	if len(cancelled) > 0 && i.IssueService.TaskService != nil {
		i.IssueService.TaskService.BroadcastCancelledTasks(ctx, util.UUIDToString(issue.WorkspaceID), cancelled)
	}
}

type mirroredBranchOwner struct {
	Name    string `json:"bug_assignee_name"`
	Env     string `json:"sync_source_env"`
	Version int64  `json:"bug_version_id"`
}

func sourceBranchScopeChanged(before, after []byte) bool {
	var old, next mirroredBranchOwner
	return json.Unmarshal(before, &old) != nil || json.Unmarshal(after, &next) != nil || old != next
}

// A blocked run has nobody in the foreground to report a changed assignment.
// Make its handoff visible without starting the old agent or moving its PR.
func branchHandoffNotice(ctx context.Context, q *db.Queries, issue db.Issue) (db.InboxItem, error) {
	var scope mirroredBranchOwner
	if err := json.Unmarshal(issue.Metadata, &scope); err != nil {
		return db.InboxItem{}, err
	}
	member, found, err := (&Importer{Queries: q}).lookupUniqueMemberByName(ctx, issue.WorkspaceID, scope.Name)
	if err != nil || !found {
		return db.InboxItem{}, err
	}
	item, err := q.CreateInboxItem(ctx, db.CreateInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, RecipientType: "member", RecipientID: member.UserID,
		Type: "agent_blocked", Severity: "action_required", Title: issue.Title,
		Body:    util.StrToText("Syndra 负责人或版本已变更，原分支等待已停止。请在任务中接手并按当前负责人的分支决定恢复，已有提交和 PR 保留。"),
		Details: []byte(`{"reason":"syndra_branch_scope_changed"}`),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.InboxItem{}, nil
	}
	return item, err
}

func (s BranchScope) origin() pgtype.UUID {
	data, _ := json.Marshal(s)
	return sourceOriginID("multica:syndra:personal-branch:v1", "", "", string(data))
}

func (s BranchScope) Marker() string { return "[syndra-branch:" + util.UUIDToString(s.origin()) + "]" }

var repoPath = regexp.MustCompile(`^[a-zA-Z0-9._-]+(/[a-zA-Z0-9._-]+)+$`)

func canonicalBranchRepo(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "git@") && !strings.Contains(raw, "://") {
		host, path, ok := strings.Cut(strings.TrimPrefix(raw, "git@"), ":")
		if ok {
			raw = "ssh://git@" + host + "/" + path
		}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh") || u.Hostname() == "" ||
		u.RawQuery != "" || u.Fragment != "" || (u.User != nil && (u.Scheme != "ssh" || u.User.String() != "git")) {
		return "", fmt.Errorf("%w: supply a credential-free HTTPS or SSH repository URL", ErrBranchInput)
	}
	path := strings.TrimSuffix(strings.Trim(u.EscapedPath(), "/"), ".git")
	if !repoPath.MatchString(path) || len(raw) > 1024 {
		return "", fmt.Errorf("%w: invalid repository path", ErrBranchInput)
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return "", fmt.Errorf("%w: invalid repository path", ErrBranchInput)
		}
	}
	host := strings.ToLower(u.Host)
	if (u.Port() == "22" && u.Scheme == "ssh") || (u.Port() == "443" && u.Scheme == "https") || (u.Port() == "80" && u.Scheme == "http") {
		host = strings.ToLower(u.Hostname())
	}
	if host == "github.com" {
		path = strings.ToLower(path)
	}
	return host + "/" + path, nil
}

// Resolve the current mirrored assignee, never the immutable issue creator
// or today's Multica agent assignee. Ambiguous names cannot share a record.
func (i *Importer) BranchScope(ctx context.Context, issue db.Issue, repo string) (BranchScope, error) {
	var m struct {
		Source  string `json:"external_source"`
		Enabled bool   `json:"multica_bug_automation"`
		mirroredBranchOwner
	}
	if issue.OriginType.String != OriginType || json.Unmarshal(issue.Metadata, &m) != nil ||
		m.Source != defaultBugProvider || !m.Enabled || m.Version <= 0 || strings.TrimSpace(m.Env) == "" {
		return BranchScope{}, fmt.Errorf("%w: missing enrollment, source environment or version ID", ErrBranchInput)
	}
	if _, ok := configuredBugAutomationAssignees(i.Config.BugAutomationAssignees)[m.Name]; !ok {
		return BranchScope{}, fmt.Errorf("%w: current assignee is outside the pilot", ErrBranchInput)
	}
	person, ok, err := i.lookupUniqueMemberByName(ctx, issue.WorkspaceID, m.Name)
	if err != nil {
		return BranchScope{}, err
	}
	if !ok {
		return BranchScope{}, fmt.Errorf("%w: current assignee is missing or ambiguous", ErrBranchInput)
	}
	key, err := canonicalBranchRepo(repo)
	if err != nil {
		return BranchScope{}, err
	}
	return BranchScope{util.UUIDToString(issue.WorkspaceID), strings.TrimSpace(m.Env), key, m.Version, util.UUIDToString(person.UserID)}, nil
}

func (s BranchScope) question(candidates []string, bug db.Issue) string {
	data, _ := json.Marshal(s)
	var meta struct {
		VersionName string `json:"bug_version_name"`
	}
	_ = json.Unmarshal(bug.Metadata, &meta)
	proposal := ""
	if len(candidates) == 1 {
		proposal = "\n唯一候选：" + candidates[0] + "\n可以直接回复「确认」。"
	}
	options := "没有匹配项，请提供实际远端分支名。"
	if len(candidates) > 0 {
		options = "\n- " + strings.Join(candidates, "\n- ")
	}
	return fmt.Sprintf("仓库：%s；来源环境：%s；版本：%s（ID %d）。\n起始任务：[Bug #%d](mention://issue/%s)\n候选分支：%s%s\n请本人回复候选分支名，或「默认 分支名」。后续只对你在这个仓库和版本的 Bug 生效。修改请另发「更换默认 分支名」。仅本单例外请在原 Bug 的分支说明下直接回复「仅本单 分支名」。\n\n%s%s -->",
		s.Repo, s.Env, strings.Join(strings.Fields(meta.VersionName), " "), s.Version, bug.Number, util.UUIDToString(bug.ID), options, proposal, scopePrefix, data)
}

func choiceTicket(issue db.Issue) (BranchScope, bool) {
	var s BranchScope
	_, line, found := strings.Cut(issue.Description.String, "\n\n"+scopePrefix)
	raw, ok := strings.CutSuffix(line, " -->")
	if !found || !ok || issue.OriginType.String != OriginType || json.Unmarshal([]byte(raw), &s) != nil ||
		s.Workspace != util.UUIDToString(issue.WorkspaceID) || s.origin() != issue.OriginID {
		return s, false
	}
	return s, true
}

func (i *Importer) BranchTicket(ctx context.Context, bug db.Issue, s BranchScope, candidates []string, actorType string, actor pgtype.UUID) (db.Issue, error) {
	find := db.GetIssueByOriginParams{WorkspaceID: bug.WorkspaceID, OriginType: pgtype.Text{String: OriginType, Valid: true}, OriginID: s.origin()}
	ticket, err := i.Queries.GetIssueByOrigin(ctx, find)
	if !errors.Is(err, pgx.ErrNoRows) {
		return ticket, err
	}
	member, err := util.ParseUUID(s.Member)
	if err != nil {
		return db.Issue{}, err
	}
	res, err := i.IssueService.Create(ctx, service.IssueCreateParams{
		WorkspaceID: bug.WorkspaceID, ProjectID: bug.ProjectID,
		Title: fmt.Sprintf("确认个人版本分支：%s · %d", s.Repo, s.Version), Description: util.StrToText(s.question(candidates, bug)),
		Status: "todo", Priority: "none", AssigneeType: pgtype.Text{String: "member", Valid: true}, AssigneeID: member,
		CreatorType: actorType, CreatorID: actor, OriginType: find.OriginType, OriginID: find.OriginID, AllowDuplicate: true,
	}, service.IssueCreateOpts{Platform: "external_import:syndra_branch"})
	if isUniqueViolation(err, "idx_issue_external_origin_unique") {
		return i.Queries.GetIssueByOrigin(ctx, find)
	}
	if err != nil {
		return db.Issue{}, err
	}
	i.notifyAssignee(ctx, res.Issue, member)
	return res.Issue, nil
}

func ValidBranch(branch string) bool {
	if branch == "" || branch == "@" || branch == "HEAD" || len(branch) > 255 || strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "refs/") ||
		strings.HasSuffix(branch, ".") || strings.ContainsAny(branch, "~^:?*[\\") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") ||
		strings.IndexFunc(branch, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || part[0] == '.' || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func branchReply(content string) (kind, branch string) {
	parts := strings.Fields(content)
	if len(parts) != 2 || !ValidBranch(parts[1]) {
		return "", ""
	}
	switch parts[0] {
	case "默认", "更换默认", "仅本单":
		return parts[0], parts[1]
	}
	return "", ""
}

func ticketReply(ticket db.Issue, content string) (string, string) {
	content = strings.TrimSpace(content)
	if ValidBranch(content) && strings.Contains(ticket.Description.String, "\n- "+content+"\n") {
		return "默认", content
	}
	if content == "确认" {
		for _, line := range strings.Split(ticket.Description.String, "\n") {
			if name, ok := strings.CutPrefix(line, "唯一候选："); ok && ValidBranch(name) {
				return "默认", name
			}
		}
	}
	return branchReply(content)
}

// Store a reference to the real member comment, not an agent's interpretation
// of it. The caller already holds the issue row lock from inserting the comment.
func ApplyBranchReply(ctx context.Context, q *db.Queries, issue db.Issue, c db.Comment) (db.Issue, bool, error) {
	if c.AuthorType != "member" {
		return issue, false, nil
	}
	s, ok := choiceTicket(issue)
	if !ok || s.Member != util.UUIDToString(c.AuthorID) || c.IssueID != issue.ID || c.WorkspaceID != issue.WorkspaceID {
		return issue, false, nil
	}
	current, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		return issue, false, err
	}
	if locked, ok := choiceTicket(current); !ok || locked != s {
		return current, false, ErrBranchInput
	}
	kind, branch := ticketReply(current, c.Content)
	if kind != "默认" && kind != "更换默认" {
		return current, false, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(current.Metadata, &m); err != nil {
		return issue, false, err
	}
	if _, exists := m[BranchDecisionKey]; exists && kind != "更换默认" {
		return current, false, fmt.Errorf("%w: 已有个人默认分支，请回复「更换默认 分支名」修改", ErrBranchInput)
	}
	value, _ := json.Marshal(util.UUIDToString(c.ID) + " " + branch)
	if _, err = q.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Key: BranchDecisionKey, Value: value}); err != nil {
		return issue, false, err
	}
	updated, err := q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Status: "done"})
	return updated, true, err
}

func ConfirmedBranch(ctx context.Context, q *db.Queries, ticket db.Issue, s BranchScope) (string, string, error) {
	if got, ok := choiceTicket(ticket); !ok || got != s {
		return "", "", fmt.Errorf("%w: confirmation issue identity changed", ErrBranchInput)
	}
	var m map[string]json.RawMessage
	var receipt string
	if err := json.Unmarshal(ticket.Metadata, &m); err != nil {
		return "", "", err
	}
	raw, ok := m[BranchDecisionKey]
	if !ok {
		return "", "", nil
	}
	if json.Unmarshal(raw, &receipt) != nil {
		return "", "", fmt.Errorf("%w: malformed confirmation receipt", ErrBranchInput)
	}
	id, branch, _ := strings.Cut(receipt, " ")
	uid, err := util.ParseUUID(id)
	if err != nil {
		return "", "", fmt.Errorf("%w: invalid confirmation receipt", ErrBranchInput)
	}
	c, err := q.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: uid, WorkspaceID: ticket.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("%w: confirmation deleted; member must post a new 更换默认 reply", ErrBranchInput)
	}
	if err != nil {
		return "", "", err
	}
	kind, value := ticketReply(ticket, c.Content)
	if c.IssueID != ticket.ID || c.AuthorType != "member" || util.UUIDToString(c.AuthorID) != s.Member || c.DeletedAt.Valid ||
		(kind != "默认" && kind != "更换默认") || value != branch {
		return "", "", fmt.Errorf("%w: confirmation changed; member must post a new 更换默认 reply", ErrBranchInput)
	}
	return branch, id, nil
}
