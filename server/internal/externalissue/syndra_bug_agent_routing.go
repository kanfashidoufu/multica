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

const bugAutomationAcceptance = "\n\n自动化验收：由工作区公用的 bug修复专家使用 multica-fixing-syndra-bugs skill。先依据工作区仓库说明和 Bug 证据定位受影响的仓库，再在各仓库的独立 worktree 修复。按当前 Syndra 人工指派人、仓库和版本解析分支：本单例外优先，否则复用本人的默认分支；未确认时共用一张分支确认单。自愈修复可直接使用 Git 提交，无需 QTB 本地提交门禁回执；仍需执行验证并遵守仓库保护规则。代码已改完且只等当前负责人确认时，将 Bug 任务设为审核中（in_review），说明待确认事项和实际合并进度。验证通过后必须取得本地验证及 CI 结果，将修复合入已确认的版本分支并推送远端，再将该版本分支合入 test 并推送远端；冲突解决后通知当前人工指派人。验证失败时提交当前修复快照到版本分支并 block 任务，通知当前人工指派人介入。仅创建 PR、进入合并队列或发布测试环境均不算完成。"

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
	return false
}

// An upstream 'resolved' event is not proof the automated fix reached its version branch.
func bugAutomationMirror(existing db.Issue, description, status string, metadata map[string]any) (string, string, []byte, error) {
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
		status = existing.Status
	}
	encoded, err := json.Marshal(metadata)
	return description, status, encoded, err
}

// Reuse the existing issue-row lock so a concurrent agent status update cannot
// be overwritten by the older snapshot used to locate an imported issue.
func (i *Importer) updateBugSyncMirror(ctx context.Context, existing db.Issue, reviewer db.Member, title, description, status, priority string, metadata map[string]any) (db.Issue, db.Issue, error) {
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
	if readable && last.Candidate != "" && last.MateID > 0 && metadata["bug_assignee_mate_id"] == last.MateID &&
		metadata["bug_assignee_name"] == last.Name && metadata["bug_assignee_fallback_reason"] == nil &&
		metadata["bug_assignee_email_candidate"] == nil && len(metadata) < maxBugMetadataKeys {
		metadata["bug_assignee_email_candidate"] = last.Candidate
	}
	description, status, encoded, err := bugAutomationMirror(current, description, status, metadata)
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
	if !bugAutomationEnrolled(current.Metadata) && reviewer.UserID.Valid &&
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
	if bugAutomationEnrolled(current.Metadata) && sourceBranchScopeChanged(current.Metadata, encoded) {
		cancelled, err = cancelBranchWaits(ctx, tx, current, "")
		if err != nil {
			return current, db.Issue{}, err
		}
		handoff, err = branchHandoffNotice(ctx, q, updated, reviewer.UserID)
		if err != nil {
			return current, db.Issue{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return current, db.Issue{}, err
	}
	if updated.AssigneeType.String == "member" && (current.AssigneeID != updated.AssigneeID || current.AssigneeType != updated.AssigneeType) {
		i.notifyAssignee(ctx, updated, updated.AssigneeID)
	}
	i.publishBranchWaitCancellation(ctx, current, cancelled)
	if handoff.ID.Valid {
		i.publishInboxNew(handoff, updated.Status)
	}
	return current, updated, nil
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

// routeNewBugToDeveloperAgentIfEligible is intentionally scoped to new Syndra
// issues. The caller stamps the complete external metadata first; only then
// does this helper reassign and enqueue the issue. This keeps the localized
// workflow out of the shared IssueService and sqlc create contracts.
func (i *Importer) routeNewBugToDeveloperAgentIfEligible(
	ctx context.Context,
	issue db.Issue,
	assignment bugDeveloperAssignment,
	status string,
) db.Issue {
	if !assignment.DeveloperAgentID.Valid || !bugStatusStartsDevelopment(status) || !bugAutomationEnrolled(issue.Metadata) {
		return issue
	}
	updated, err := i.routeNewBugToDeveloperAgent(ctx, issue, assignment)
	if err != nil {
		i.warn("external bug sync: developer agent routing failed",
			"issue_id", util.UUIDToString(issue.ID),
			"reviewer_id", util.UUIDToString(assignment.Reviewer.UserID),
			"developer_agent_id", util.UUIDToString(assignment.DeveloperAgentID),
			"error", err,
		)
	}
	return updated
}

func (i *Importer) routeNewBugToDeveloperAgent(
	ctx context.Context,
	issue db.Issue,
	assignment bugDeveloperAssignment,
) (db.Issue, error) {
	if i.IssueService == nil || i.IssueService.TaskService == nil {
		return issue, errors.New("task service unavailable")
	}

	updated, err := i.Queries.UpdateIssue(ctx, db.UpdateIssueParams{
		ID:            issue.ID,
		AssigneeType:  pgtype.Text{String: "agent", Valid: true},
		AssigneeID:    assignment.DeveloperAgentID,
		StartDate:     issue.StartDate,
		DueDate:       issue.DueDate,
		ParentIssueID: issue.ParentIssueID,
		ProjectID:     issue.ProjectID,
		Stage:         issue.Stage,
	})
	if err != nil {
		return issue, fmt.Errorf("assign developer agent: %w", err)
	}
	i.publishExternalIssueUpdated(ctx, issue, updated, "external_bug_agent_routing")

	if _, err := i.IssueService.TaskService.EnqueueTaskForIssueByActor(
		ctx,
		updated,
		assignment.Reviewer.UserID,
	); err != nil {
		// A failed enqueue must leave the imported bug actionable by its human.
		restored, restoreErr := i.Queries.UpdateIssue(ctx, db.UpdateIssueParams{
			ID:            updated.ID,
			AssigneeType:  issue.AssigneeType,
			AssigneeID:    issue.AssigneeID,
			StartDate:     updated.StartDate,
			DueDate:       updated.DueDate,
			ParentIssueID: updated.ParentIssueID,
			ProjectID:     updated.ProjectID,
			Stage:         updated.Stage,
		})
		if restoreErr != nil {
			return updated, fmt.Errorf("enqueue developer agent: %w; restore member: %v", err, restoreErr)
		}
		i.publishExternalIssueUpdated(ctx, updated, restored, "external_bug_agent_routing_failed")
		return restored, fmt.Errorf("enqueue developer agent: %w", err)
	}
	i.info("external bug sync: developer agent routed",
		"issue_id", util.UUIDToString(updated.ID),
		"reviewer_id", util.UUIDToString(assignment.Reviewer.UserID),
		"developer_agent_id", util.UUIDToString(assignment.DeveloperAgentID),
	)
	return updated, nil
}

func bugStatusStartsDevelopment(status string) bool {
	return status == "todo" || status == "in_progress"
}
