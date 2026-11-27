package externalissue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/projectaccess"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type bugDeveloperAssignment struct {
	Reviewer         db.Member
	FallbackReason   string
	DeveloperAgentID pgtype.UUID
	CandidateCount   int
}

// The pilot follows the existing exact member-name resolution contract. A
// workspace-owner fallback must never make an unrelated bug eligible.
const bugAutomationPilotAssignee = "王宁"

const bugAutomationMetadataKey = "multica_bug_automation"

const BugExpertName = "bug修复专家"

// Workspace visibility alone does not grant permission to run an agent.
func IsWorkspaceBugExpert(ctx context.Context, q *db.Queries, agent db.Agent, workspaceID pgtype.UUID) (bool, error) {
	if agent.WorkspaceID != workspaceID || agent.Name != BugExpertName || agent.Kind != "user" ||
		agent.ArchivedAt.Valid || !agent.RuntimeID.Valid || agent.PermissionMode != "public_to" {
		return false, nil
	}
	targets, err := q.ListAgentInvocationTargets(ctx, agent.ID)
	if err != nil {
		return false, err
	}
	for _, target := range targets {
		if target.TargetType == "workspace" && target.TargetID == workspaceID {
			return true, nil
		}
	}
	return false, nil
}

const bugAutomationAcceptance = "\n\n自动化验收：由工作区公用的 bug修复专家使用 multica-fixing-syndra-bugs skill。先依据工作区仓库说明和 Bug 证据定位受影响的仓库，通过 SSH 拉取代码并在各仓库的独立 worktree 修复。按当前 Syndra 人工指派人、仓库和版本解析分支：本单例外优先，否则复用本人的默认分支；未确认时按分支解析器提示由本人确认；支持原 Bug 回复时，不要求本人另行结单或手动恢复。自愈修复可直接使用 Git 提交，无需 QTB 本地提交门禁回执；仍需执行验证并遵守仓库保护规则。代码已改完且只等当前负责人确认时，将 Bug 任务设为审核中（in_review），说明待确认事项和实际合并进度。验证通过后必须取得本地验证及 CI 结果，将修复合入已确认的版本分支并推送远端，再将该版本分支合入 test 并推送远端；冲突解决后通知当前人工指派人。验证失败时提交当前修复快照到版本分支并 block 任务，通知当前人工指派人介入。仅创建 PR、进入合并队列或发布测试环境均不算完成。"

// The v1 sender repeats canonical fields in its optional metadata map. A
// terminal snapshot can fill the last slot by adding solver information.
func hasBugAutomationSlot(metadata map[string]any) bool {
	if len(metadata) < maxBugMetadataKeys {
		return true
	}
	for _, pair := range [][2]string{{"source", "external_source"}, {"syndra_env", "sync_source_env"}, {"syndra_role", "bug_role"}, {"syndra_entity_type", "bug_entity_type"}, {"syndra_version_name", "bug_version_name"}} {
		alias, ok := metadata[pair[0]].(string)
		if ok && alias == metadata[pair[1]] {
			delete(metadata, pair[0])
			return true
		}
	}
	// A full v2 snapshot can exhaust the budget before optional aliases are
	// copied. Preserve the verified owner and enrollment, not a duplicate key.
	if metadata["sync_schema_version"] == syndraBugV2 {
		if key, ok := metadata["bug_external_key"].(string); ok && key != "" && key == metadata["external_key"] {
			delete(metadata, "bug_external_key")
			return true
		}
	}
	// Both fields are stamped from the same structured provider. Keep the
	// source key when a dense snapshot has no other redundant aliases.
	if source, ok := metadata["external_source"].(string); ok && source != "" && metadata["external_provider"] == source {
		delete(metadata, "external_provider")
		return true
	}
	return false
}

// Nonterminal snapshots preserve the enrolled issue's local delivery stage.
func bugAutomationMirror(existing db.Issue, description string, source bugSyncState, metadata map[string]any) (string, string, []byte, error) {
	status := source.status
	delete(metadata, bugAutomationMetadataKey)
	var previous map[string]any
	if err := json.Unmarshal(existing.Metadata, &previous); err != nil {
		return "", "", nil, fmt.Errorf("read bug automation state: %w", err)
	}
	if previous[bugAutomationMetadataKey] == true {
		if !hasBugAutomationSlot(metadata) {
			return "", "", nil, errors.New("bug metadata is full; cannot preserve automation state")
		}
		metadata[bugAutomationMetadataKey] = true
		description += bugAutomationAcceptance
		if !source.terminalBySyndra {
			status = existing.Status
		}
	}
	encoded, err := json.Marshal(metadata)
	return description, status, encoded, err
}

// Reuse the existing issue-row lock so a concurrent agent status update cannot
// be overwritten by the older snapshot used to locate an imported issue.
func (i *Importer) updateBugSyncMirror(ctx context.Context, existing db.Issue, assignment bugDeveloperAssignment, title, description string, source bugSyncState, priority string, metadata map[string]any) (db.Issue, db.Issue, error) {
	reviewer := assignment.Reviewer
	tx, err := i.IssueService.TxStarter.Begin(ctx)
	if err != nil {
		return existing, db.Issue{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := i.Queries.WithTx(tx)
	current, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: existing.ID, WorkspaceID: existing.WorkspaceID})
	if err != nil {
		return existing, db.Issue{}, err
	}
	// A retry from another Syndra worker must not roll back a newer snapshot.
	var last struct {
		UpdatedAt string `json:"bug_updated_at"`
		MateID    int64  `json:"bug_assignee_mate_id"`
		Name      string `json:"bug_assignee_name"`
		Candidate string `json:"bug_assignee_email_candidate"`
	}
	// Older mirrors may not have typed person fields. An unreadable hint is
	// not an admission decision and must not prevent refreshing the issue.
	readable := json.Unmarshal(current.Metadata, &last) == nil
	if next, ok := metadata["bug_updated_at"].(string); ok {
		before, e1 := time.Parse(time.RFC3339Nano, last.UpdatedAt)
		after, e2 := time.Parse(time.RFC3339Nano, next)
		if e1 == nil && e2 == nil && after.Before(before) {
			return current, current, nil
		}
	}
	// Once provisioned, later syncs match the workspace member locally.
	// Keep the suggestion on this Bug until its source assignee changes.
	if readable && metadata["sync_schema_version"] != syndraBugV2 && last.Candidate != "" && last.MateID > 0 && metadata["bug_assignee_mate_id"] == last.MateID &&
		metadata["bug_assignee_name"] == last.Name && metadata["bug_assignee_fallback_reason"] == nil &&
		metadata["bug_assignee_email_candidate"] == nil && len(metadata) < maxBugMetadataKeys {
		metadata["bug_assignee_email_candidate"] = last.Candidate
	}
	description, status, encoded, err := bugAutomationMirror(current, description, source, metadata)
	if err != nil {
		return current, db.Issue{}, err
	}
	updated, err := q.UpdateIssueFromExternalSync(ctx, db.UpdateIssueFromExternalSyncParams{
		Title: title, Description: util.StrToText(description), Status: status,
		Priority: priority, Metadata: encoded, ID: current.ID, WorkspaceID: current.WorkspaceID,
	})
	if err != nil {
		return current, db.Issue{}, err
	}
	// Enrolled issues keep their execution owner; the source human is mirrored
	// in metadata and drives branch confirmation and review handoff.
	repairable := assignment.DeveloperAgentID.Valid && bugStatusStartsDevelopment(source.status) && bugStatusStartsDevelopment(status) && i.IssueService.TaskService != nil
	if !bugAutomationEnrolled(current.Metadata) && reviewer.UserID.Valid && !(repairable && current.AssigneeType.String == "agent") &&
		(current.AssigneeID != reviewer.UserID || current.AssigneeType.String != "member") {
		hidden, err := projectaccess.ForMember(ctx, q, reviewer)
		if err != nil {
			return current, db.Issue{}, err
		}
		if hidden.CanSeeProject(updated.ProjectID) {
			updated, err = q.UpdateIssue(ctx, db.UpdateIssueParams{
				ID: updated.ID, AssigneeType: util.StrToText("member"), AssigneeID: reviewer.UserID,
				ParentIssueID: updated.ParentIssueID, ProjectID: updated.ProjectID, Stage: updated.Stage,
				StartDate: updated.StartDate, DueDate: updated.DueDate,
			})
			if err != nil {
				return current, db.Issue{}, err
			}
		} else {
			i.warn("external bug sync: assignee cannot access issue; keeping local assignment",
				"issue_id", util.UUIDToString(updated.ID), "user_id", util.UUIDToString(reviewer.UserID))
		}
	}
	var cancelled []db.AgentTaskQueue
	var handoff db.InboxItem
	handoffChanged := !source.terminalBySyndra && bugAutomationEnrolled(current.Metadata) && sourceBranchScopeChanged(current.Metadata, encoded)
	if source.terminalBySyndra {
		// This source command stops work, unlike an ordinary Multica status edit.
		// Settle the cancellations in the same transaction as the mirror write.
		if err = q.DisableIssueWakeups(ctx, updated.ID); err != nil {
			return current, db.Issue{}, err
		}
		cancelled, err = q.CancelAgentTasksByIssue(ctx, updated.ID)
		if err != nil {
			return current, db.Issue{}, err
		}
		for _, task := range cancelled {
			if !task.StartedAt.Valid {
				continue
			}
			if err = q.MarkPiTaskProcessCleanupPending(ctx, task.ID); err != nil {
				return current, db.Issue{}, err
			}
		}
		if err = service.SettleTerminalTaskState(ctx, q, cancelled...); err != nil {
			return current, db.Issue{}, err
		}
	} else if handoffChanged {
		cancelled, err = cancelBranchWaits(ctx, tx, current, "")
		if err != nil {
			return current, db.Issue{}, err
		}
		handoff, err = branchHandoffNotice(ctx, q, updated, reviewer.UserID)
		if err != nil {
			return current, db.Issue{}, err
		}
	}
	if repairable {
		busy, err := q.HasConflictingBugRepairTask(ctx, bugRepairConflict(updated, assignment.DeveloperAgentID))
		if err != nil {
			return current, db.Issue{}, err
		}
		repairable = !busy
	}
	if err := tx.Commit(ctx); err != nil {
		return current, db.Issue{}, err
	}
	if updated.AssigneeType.String == "member" && (current.AssigneeID != updated.AssigneeID || current.AssigneeType != updated.AssigneeType) {
		i.notifyAssignee(ctx, updated, updated.AssigneeID)
	}
	if handoffChanged && reviewer.UserID.Valid && IsSyndraBug(updated) && metadata["bug_assignee_fallback_reason"] == nil {
		if err := i.subscribeExpertReviewer(ctx, updated, reviewer); err != nil {
			i.warn("Syndra reviewer subscription failed", "issue_id", util.UUIDToString(updated.ID), "error", err)
		}
	}
	i.publishCancelledBugTasks(ctx, current, cancelled)
	if source.terminalBySyndra && (current.Status != updated.Status || len(cancelled) > 0) {
		i.info("external bug sync: source ended issue", "issue_id", util.UUIDToString(updated.ID), "source_status", metadata["bug_status"], "cancelled_runs", len(cancelled))
	}
	if handoff.ID.Valid {
		i.publishInboxNew(handoff, updated.Status)
	}
	if repairable {
		routed, routeErr := i.routeBugToExpert(ctx, updated, assignment)
		if routeErr != nil {
			return current, updated, fmt.Errorf("route synchronized bug to expert: %w", routeErr)
		}
		if routed.AssigneeID == assignment.DeveloperAgentID && routed.AssigneeType.String == "agent" && !handoffChanged {
			if err := i.subscribeExpertReviewer(ctx, routed, reviewer); err != nil {
				i.warn("Syndra reviewer subscription failed", "issue_id", util.UUIDToString(routed.ID), "error", err)
			}
		}
		updated = routed
	}
	return current, updated, nil
}

func (i *Importer) subscribeExpertReviewer(ctx context.Context, bug db.Issue, member db.Member) error {
	ws, err := i.Queries.GetWorkspace(ctx, bug.WorkspaceID)
	if err != nil || ws.Slug != ExpertWorkspace {
		return err
	}
	visible, err := projectaccess.ForMember(ctx, i.Queries, member)
	if err != nil || !visible.CanSeeProject(bug.ProjectID) {
		return err
	}
	added, err := i.Queries.AddIssueSubscriber(ctx, db.AddIssueSubscriberParams{IssueID: bug.ID, UserType: "member", UserID: member.UserID, Reason: "assignee"})
	if err == nil && added > 0 {
		i.publishSubscriberAdded(bug, member.UserID)
	}
	return err
}

func configuredBugAutomationAssignees(raw string) map[string]struct{} {
	if raw == "" {
		raw = bugAutomationPilotAssignee
	}
	allowed := make(map[string]struct{})
	for _, value := range strings.Split(raw, ",") {
		if name := strings.TrimSpace(value); name != "" {
			allowed[name] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		allowed[bugAutomationPilotAssignee] = struct{}{}
	}
	return allowed
}

func bugAutomationPilotEligible(provider string, item BugSyncItem, fallbackReason string) bool {
	return bugAutomationPilotEligibleForNames(provider, item, fallbackReason, configuredBugAutomationAssignees(""))
}

func bugAutomationPilotEligibleForNames(provider string, item BugSyncItem, fallbackReason string, allowed map[string]struct{}) bool {
	_, allowedAssignee := allowed[bugPersonName(bugAssigneePerson(item))]
	return !item.ambiguousAutomationOwner && provider == defaultBugProvider && fallbackReason == "" &&
		allowedAssignee
}

func bugAutomationEnrolled(metadata []byte) bool {
	var values map[string]any
	return json.Unmarshal(metadata, &values) == nil && values[bugAutomationMetadataKey] == true
}

func (i *Importer) listWorkspaceBugExperts(ctx context.Context, payload BugSyncPayload, workspaceID pgtype.UUID) []db.Agent {
	agents, err := i.Queries.ListAgents(ctx, workspaceID)
	if err == nil {
		var experts []db.Agent
		for _, agent := range agents {
			var shared bool
			shared, err = IsWorkspaceBugExpert(ctx, i.Queries, agent, workspaceID)
			if err != nil {
				break
			}
			if shared {
				experts = append(experts, agent)
			}
		}
		if err == nil {
			return experts
		}
	}
	i.warn("external bug sync: developer agent inventory unavailable",
		"provider", bugProvider(payload),
		"workspace_id", util.UUIDToString(workspaceID),
		"event_id", payload.EventID,
		"error", err,
	)
	return nil
}

func (i *Importer) resolveBugDeveloperAssignmentForItem(
	workspaceID pgtype.UUID,
	provider string,
	recordID string,
	item BugSyncItem,
	assignee db.Member,
	fallbackReason string,
	experts []db.Agent,
) bugDeveloperAssignment {
	assignment := bugDeveloperAssignment{Reviewer: assignee, FallbackReason: fallbackReason}
	if !projectaccess.IsGuestRole(assignee.Role) && bugAutomationPilotEligibleForNames(provider, item, fallbackReason, configuredBugAutomationAssignees(i.Config.BugAutomationAssignees)) {
		assignment.CandidateCount = len(experts)
		if len(experts) == 1 {
			assignment.DeveloperAgentID = experts[0].ID
		}
	}
	i.info("external bug sync: developer agent routing resolved",
		"provider", provider,
		"workspace_id", util.UUIDToString(workspaceID),
		"record_id", recordID,
		"external_key", item.ExternalKey,
		"bug_id", item.BugID,
		"reviewer_id", util.UUIDToString(assignment.Reviewer.UserID),
		"developer_agent_id", util.UUIDToString(assignment.DeveloperAgentID),
		"developer_agent_candidate_count", assignment.CandidateCount,
	)
	return assignment
}

var errBugRouteChanged = errors.New("bug no longer needs expert dispatch")

func bugRepairConflict(issue db.Issue, expert pgtype.UUID) db.HasConflictingBugRepairTaskParams {
	p := db.HasConflictingBugRepairTaskParams{IssueID: issue.ID}
	if issue.AssigneeType.String == "agent" && issue.AssigneeID == expert {
		p.AlreadyAssignedExpert = expert
	}
	return p
}

func (i *Importer) routeBugToExpert(ctx context.Context, original db.Issue, assignment bugDeveloperAssignment) (db.Issue, error) {
	target := original
	target.AssigneeType, target.AssigneeID = util.StrToText("agent"), assignment.DeveloperAgentID
	var updated db.Issue
	task, err := i.IssueService.TaskService.EnqueueAssignedIssueTask(ctx, target, assignment.Reviewer.UserID, func(q *db.Queries) error {
		current, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: original.ID, WorkspaceID: original.WorkspaceID})
		if err != nil {
			return err
		}
		// A newer webhook or a local workflow change owns the newer snapshot.
		if current.Revision != original.Revision {
			return errBugRouteChanged
		}
		agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: current.WorkspaceID, ID: target.AssigneeID})
		if err != nil {
			return err
		}
		shared, err := IsWorkspaceBugExpert(ctx, q, agent, current.WorkspaceID)
		if err != nil {
			return err
		}
		if !shared {
			return errors.New("workspace bug expert is no longer available")
		}
		busy, err := q.HasConflictingBugRepairTask(ctx, bugRepairConflict(current, agent.ID))
		if err != nil {
			return err
		}
		if busy {
			return errBugRouteChanged
		}
		if !bugAutomationEnrolled(current.Metadata) {
			var m map[string]any
			if err = json.Unmarshal(current.Metadata, &m); err != nil {
				return err
			}
			if !hasBugAutomationSlot(m) {
				return errors.New("cannot store bug automation enrollment")
			}
			m[bugAutomationMetadataKey] = true
			raw, err := json.Marshal(m)
			if err != nil {
				return err
			}
			current, err = q.UpdateIssueFromExternalSync(ctx, db.UpdateIssueFromExternalSyncParams{ID: current.ID, WorkspaceID: current.WorkspaceID, Title: current.Title, Description: util.StrToText(current.Description.String + bugAutomationAcceptance), Status: current.Status, Priority: current.Priority, Metadata: raw})
			if err != nil {
				return err
			}
		}
		updated, err = q.UpdateIssue(ctx, db.UpdateIssueParams{ID: current.ID, AssigneeType: target.AssigneeType, AssigneeID: target.AssigneeID, StartDate: current.StartDate, DueDate: current.DueDate, ParentIssueID: current.ParentIssueID, ProjectID: current.ProjectID, Stage: current.Stage})
		return err
	})
	if errors.Is(err, errBugRouteChanged) {
		return i.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: original.WorkspaceID, ID: original.ID})
	}
	if err != nil {
		return original, err
	}
	i.info("external bug sync: developer agent routed", "issue_id", util.UUIDToString(updated.ID), "developer_agent_id", util.UUIDToString(updated.AssigneeID), "reviewer_id", util.UUIDToString(assignment.Reviewer.UserID), "task_id", util.UUIDToString(task.ID))
	return updated, nil
}

func bugStatusStartsDevelopment(status string) bool {
	return status == "todo" || status == "in_progress"
}
