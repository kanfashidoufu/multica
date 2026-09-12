package externalissue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type bugDeveloperAssignment struct {
	ReviewerID       pgtype.UUID
	DeveloperAgentID pgtype.UUID
	CandidateCount   int
}

// The pilot follows the existing exact member-name resolution contract. A
// workspace-owner fallback must never make an unrelated bug eligible.
const bugAutomationPilotAssignee = "王宁"

const bugAutomationMetadataKey = "multica_bug_automation"

const bugAutomationAcceptance = "\n\n自动化验收：使用 multica-fixing-syndra-bugs skill。先确认每个仓库对应的版本分支；无法确认时请当前人工指派人确认。验证通过后必须取得本地验证及 CI 结果，将修复合入已确认的版本分支并推送远端，再将该版本分支合入 test 并推送远端；冲突解决后通知当前人工指派人。验证失败时提交当前修复快照到版本分支并 block 任务，通知当前人工指派人介入。仅创建 PR、进入合并队列或发布测试环境均不算完成。"

// Keep the pilot's delivery state local: an upstream 'resolved' event is not
// proof that its fix reached the release branch. Other imports keep mirroring.
func bugAutomationMirror(existing db.Issue, description, status string, metadata map[string]any) (string, string, []byte, error) {
	delete(metadata, bugAutomationMetadataKey)
	var previous map[string]any
	if err := json.Unmarshal(existing.Metadata, &previous); err != nil {
		return "", "", nil, fmt.Errorf("read bug automation state: %w", err)
	}
	if previous[bugAutomationMetadataKey] == true {
		if len(metadata) >= maxBugMetadataKeys {
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
func (i *Importer) updateBugSyncMirror(ctx context.Context, existing db.Issue, title, description, status, priority string, metadata map[string]any) (db.Issue, db.Issue, error) {
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
	if err := tx.Commit(ctx); err != nil {
		return current, db.Issue{}, err
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
	return provider == defaultBugProvider && fallbackReason == "" &&
		allowedAssignee
}

func bugAutomationEnrolled(metadata []byte) bool {
	var values map[string]any
	return json.Unmarshal(metadata, &values) == nil && values[bugAutomationMetadataKey] == true
}

func (i *Importer) listBugDeveloperAgents(ctx context.Context, payload BugSyncPayload, workspaceID pgtype.UUID) []db.Agent {
	agents, err := i.Queries.ListAgents(ctx, workspaceID)
	if err == nil {
		return agents
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
	reviewerID pgtype.UUID,
	fallbackReason string,
	agents []db.Agent,
) bugDeveloperAssignment {
	assignment := bugDeveloperAssignment{ReviewerID: reviewerID}
	if bugAutomationPilotEligibleForNames(provider, item, fallbackReason, configuredBugAutomationAssignees(i.Config.BugAutomationAssignees)) {
		assignment = resolveBugDeveloperAssignment(reviewerID, agents)
	}
	i.info("external bug sync: developer agent routing resolved",
		"provider", provider,
		"workspace_id", util.UUIDToString(workspaceID),
		"record_id", recordID,
		"external_key", item.ExternalKey,
		"bug_id", item.BugID,
		"reviewer_id", util.UUIDToString(assignment.ReviewerID),
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
			"reviewer_id", util.UUIDToString(assignment.ReviewerID),
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
		assignment.ReviewerID,
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
		"reviewer_id", util.UUIDToString(assignment.ReviewerID),
		"developer_agent_id", util.UUIDToString(assignment.DeveloperAgentID),
	)
	return updated, nil
}

func resolveBugDeveloperAssignment(reviewerID pgtype.UUID, agents []db.Agent) bugDeveloperAssignment {
	assignment := bugDeveloperAssignment{ReviewerID: reviewerID}
	var candidate db.Agent
	for _, agent := range agents {
		if !agent.OwnerID.Valid || agent.OwnerID != reviewerID ||
			!agent.RuntimeID.Valid || agent.ArchivedAt.Valid {
			continue
		}
		assignment.CandidateCount++
		candidate = agent
	}
	if assignment.CandidateCount == 1 {
		assignment.DeveloperAgentID = candidate.ID
	}
	return assignment
}

func bugStatusStartsDevelopment(status string) bool {
	return status == "todo" || status == "in_progress"
}
