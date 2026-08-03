package externalissue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	defaultRequirementProvider = "syndra"
	defaultRequirementStatus   = "todo"
)

var (
	ErrRequirementWorkspaceNotConfigured = errors.New("Syndra requirement workspace is not configured")
	ErrMissingExecutionPrompt            = errors.New("execution_prompt is required")
	ErrMissingExecutorID                 = errors.New("executor_id is required")
	ErrInvalidExecutorID                 = errors.New("executor_id must be a valid Multica agent UUID")
	ErrMissingRequirementOwnerID         = errors.New("owner_id is required")
	ErrMissingProductIterationID         = errors.New("product_iteration_id is required")
	ErrMissingProjectRecordURL           = errors.New("project_record_url is required")
	ErrRequirementExecutorNotFound       = errors.New("executor_id does not identify an agent in the requirement workspace")
	ErrRequirementOwnerNotMember         = errors.New("the executor agent owner is not a member of the requirement workspace")
	ErrRequirementAgentNotReady          = errors.New("the executor agent is not ready for automatic development")
	ErrRequirementExecutorConflict       = errors.New("the idempotent requirement issue is already assigned to another executor")
)

// RequirementSyncPayload contains the Syndra fields Multica currently uses.
// Unknown fields remain accepted so Syndra can evolve its payload independently.
// Model and ReasoningEffort are captured only to log that they were ignored;
// runtime configuration remains owned by the Multica agent.
type RequirementSyncPayload struct {
	ID                          string          `json:"id"`
	Title                       string          `json:"title"`
	Description                 string          `json:"description,omitempty"`
	Size                        string          `json:"size,omitempty"`
	OwnerID                     string          `json:"owner_id"`
	ProductIterationID          int64           `json:"product_iteration_id"`
	ProjectRecordURL            string          `json:"project_record_url"`
	DevelopmentRole             string          `json:"development_role,omitempty"`
	ExecutionPrompt             string          `json:"execution_prompt"`
	State                       string          `json:"state,omitempty"`
	LaneID                      string          `json:"lane_id,omitempty"`
	LaneType                    string          `json:"lane_type,omitempty"`
	ExecutorKind                string          `json:"executor_kind,omitempty"`
	ExecutorID                  string          `json:"executor_id"`
	DispatchKey                 string          `json:"dispatch_key,omitempty"`
	AttemptID                   string          `json:"attempt_id,omitempty"`
	ProviderRunID               string          `json:"provider_run_id,omitempty"`
	CurrentAttempt              json.RawMessage `json:"current_attempt,omitempty"`
	ObserverNotificationChannel string          `json:"observer_notification_channel,omitempty"`
	ObserverNotificationID      string          `json:"observer_notification_id,omitempty"`
	ObserverDispatchToken       string          `json:"observer_notification_dispatch_token,omitempty"`
	Model                       json.RawMessage `json:"model,omitempty"`
	ReasoningEffort             json.RawMessage `json:"reasoning_effort,omitempty"`
}

type requirementIssueContent struct {
	Title         string
	Description   string
	DispatchKey   string
	AttemptID     string
	ProviderRunID string
}

type requirementCurrentAttempt struct {
	ID            string `json:"id"`
	AttemptID     string `json:"attempt_id"`
	DispatchKey   string `json:"dispatch_key"`
	ProviderRunID string `json:"provider_run_id"`
}

type RequirementSyncResult struct {
	Issue              db.Issue
	Existing           bool
	Provider           string
	SourceRecordID     string
	ProductIterationID int64
	ProjectRecordURL   string
}

func DecodeRequirementSyncRequest(body io.Reader) (RequirementSyncPayload, error) {
	var payload RequirementSyncPayload
	dec := json.NewDecoder(body)
	if err := dec.Decode(&payload); err != nil {
		return payload, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return payload, errors.New("request body must contain exactly one JSON object")
		}
		return payload, err
	}
	return payload, nil
}

func (i *Importer) ImportRequirementSync(ctx context.Context, payload RequirementSyncPayload) (RequirementSyncResult, error) {
	cfg := i.Config.withDefaults()
	if cfg.WebhookToken == "" {
		return RequirementSyncResult{}, ErrNotConfigured
	}
	if i.Queries == nil || i.IssueService == nil {
		return RequirementSyncResult{}, errors.New("external issue importer is not wired")
	}
	if cfg.RequirementWorkspaceID == "" {
		i.warn("external requirement sync: workspace configuration missing",
			"provider", defaultRequirementProvider,
			"source_record_id", strings.TrimSpace(payload.ID),
		)
		return RequirementSyncResult{}, ErrRequirementWorkspaceNotConfigured
	}
	workspaceID, err := util.ParseUUID(cfg.RequirementWorkspaceID)
	if err != nil {
		i.warn("external requirement sync: workspace configuration invalid",
			"provider", defaultRequirementProvider,
			"workspace_id", cfg.RequirementWorkspaceID,
			"source_record_id", strings.TrimSpace(payload.ID),
			"error", err,
		)
		return RequirementSyncResult{}, fmt.Errorf("%w: invalid workspace UUID", ErrRequirementWorkspaceNotConfigured)
	}

	payload = normalizeRequirementSyncPayload(payload)
	executorID, err := validateRequirementSyncPayload(payload)
	if err != nil {
		i.warn("external requirement sync: payload validation failed",
			"provider", defaultRequirementProvider,
			"workspace_id", util.UUIDToString(workspaceID),
			"source_record_id", payload.ID,
			"product_iteration_id", payload.ProductIterationID,
			"executor_id", payload.ExecutorID,
			"error", err,
		)
		return RequirementSyncResult{}, err
	}

	content := buildRequirementIssueContent(payload)
	metadata := requirementIssueMetadata(payload, content)
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return RequirementSyncResult{}, fmt.Errorf("marshal Syndra requirement metadata: %w", err)
	}
	originID := sourceOriginID(defaultRequirementProvider, "requirement", "", payload.ID)
	i.info("external requirement sync: payload validated",
		"provider", defaultRequirementProvider,
		"workspace_id", util.UUIDToString(workspaceID),
		"source_record_id", payload.ID,
		"product_iteration_id", payload.ProductIterationID,
		"executor_id", payload.ExecutorID,
		"owner_id", payload.OwnerID,
		"title", content.Title,
		"dispatch_key", content.DispatchKey,
		"attempt_id", content.AttemptID,
		"provider_run_id", content.ProviderRunID,
		"execution_prompt_bytes", len([]byte(payload.ExecutionPrompt)),
		"project_record_url", payload.ProjectRecordURL,
		"origin_id", util.UUIDToString(originID),
		"metadata_key_count", len(metadata),
		"model_field_ignored", len(payload.Model) > 0,
		"reasoning_effort_field_ignored", len(payload.ReasoningEffort) > 0,
	)

	existing, err := i.Queries.GetIssueByOrigin(ctx, db.GetIssueByOriginParams{
		WorkspaceID: workspaceID,
		OriginType:  pgtype.Text{String: OriginType, Valid: true},
		OriginID:    originID,
	})
	if err == nil {
		return i.updateExistingRequirementIssue(ctx, existing, payload, content, executorID, metadataBytes)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		i.warn("external requirement sync: existing issue lookup failed",
			"provider", defaultRequirementProvider,
			"workspace_id", util.UUIDToString(workspaceID),
			"source_record_id", payload.ID,
			"origin_id", util.UUIDToString(originID),
			"error", err,
		)
		return RequirementSyncResult{}, fmt.Errorf("lookup existing Syndra requirement issue: %w", err)
	}

	agent, err := i.resolveRequirementExecutor(ctx, workspaceID, executorID, payload)
	if err != nil {
		return RequirementSyncResult{}, err
	}
	createOpts := service.IssueCreateOpts{
		ActorID:          util.UUIDToString(agent.OwnerID),
		AnalyticsAgentID: util.UUIDToString(agent.ID),
		Platform:         "external_import:syndra_requirement",
	}
	if i.BroadcastPayload != nil {
		createOpts.BroadcastPayload = func(issue db.Issue, attachments []db.Attachment, _ []db.IssueLabel) map[string]any {
			return i.BroadcastPayload(ctx, issue, attachments)
		}
	}

	i.info("external requirement sync: issue creation started",
		"provider", defaultRequirementProvider,
		"workspace_id", util.UUIDToString(workspaceID),
		"source_record_id", payload.ID,
		"executor_id", util.UUIDToString(agent.ID),
		"creator_id", util.UUIDToString(agent.OwnerID),
		"status", defaultRequirementStatus,
	)
	res, err := i.IssueService.Create(ctx, service.IssueCreateParams{
		WorkspaceID:    workspaceID,
		Title:          content.Title,
		Description:    util.StrToText(content.Description),
		Status:         defaultRequirementStatus,
		Priority:       defaultPriority,
		AssigneeType:   pgtype.Text{String: "agent", Valid: true},
		AssigneeID:     agent.ID,
		CreatorType:    "member",
		CreatorID:      agent.OwnerID,
		OriginType:     pgtype.Text{String: OriginType, Valid: true},
		OriginID:       originID,
		AllowDuplicate: true,
	}, createOpts)
	if isUniqueViolation(err, "idx_issue_external_origin_unique") {
		existing, lookupErr := i.Queries.GetIssueByOrigin(ctx, db.GetIssueByOriginParams{
			WorkspaceID: workspaceID,
			OriginType:  pgtype.Text{String: OriginType, Valid: true},
			OriginID:    originID,
		})
		if lookupErr == nil {
			i.info("external requirement sync: issue creation raced idempotent request",
				"provider", defaultRequirementProvider,
				"workspace_id", util.UUIDToString(workspaceID),
				"source_record_id", payload.ID,
				"issue_id", util.UUIDToString(existing.ID),
			)
			return i.updateExistingRequirementIssue(ctx, existing, payload, content, executorID, metadataBytes)
		}
	}
	if err != nil {
		i.warn("external requirement sync: issue creation failed",
			"provider", defaultRequirementProvider,
			"workspace_id", util.UUIDToString(workspaceID),
			"source_record_id", payload.ID,
			"executor_id", util.UUIDToString(agent.ID),
			"creator_id", util.UUIDToString(agent.OwnerID),
			"error", err,
		)
		return RequirementSyncResult{}, err
	}
	i.info("external requirement sync: issue created",
		"provider", defaultRequirementProvider,
		"workspace_id", util.UUIDToString(workspaceID),
		"source_record_id", payload.ID,
		"issue_id", util.UUIDToString(res.Issue.ID),
		"issue_number", res.Issue.Number,
		"executor_id", util.UUIDToString(agent.ID),
		"creator_id", util.UUIDToString(agent.OwnerID),
	)

	updated, err := i.stampRequirementIssue(ctx, res.Issue, payload, content, metadataBytes, false)
	if err != nil {
		return RequirementSyncResult{}, err
	}
	if err := i.ensureRequirementTask(ctx, updated, agent.ID, payload.ID); err != nil {
		return RequirementSyncResult{}, err
	}
	i.info("external requirement sync: completed",
		"provider", defaultRequirementProvider,
		"workspace_id", util.UUIDToString(updated.WorkspaceID),
		"source_record_id", payload.ID,
		"issue_id", util.UUIDToString(updated.ID),
		"existing", false,
	)
	return newRequirementSyncResult(updated, payload, false), nil
}

func normalizeRequirementSyncPayload(payload RequirementSyncPayload) RequirementSyncPayload {
	payload.ID = strings.TrimSpace(payload.ID)
	payload.Title = strings.TrimSpace(payload.Title)
	payload.OwnerID = strings.TrimSpace(payload.OwnerID)
	payload.ProjectRecordURL = strings.TrimSpace(payload.ProjectRecordURL)
	payload.ExecutorID = strings.TrimSpace(payload.ExecutorID)
	payload.Size = strings.TrimSpace(payload.Size)
	payload.DevelopmentRole = strings.TrimSpace(payload.DevelopmentRole)
	payload.State = strings.TrimSpace(payload.State)
	payload.LaneID = strings.TrimSpace(payload.LaneID)
	payload.LaneType = strings.TrimSpace(payload.LaneType)
	payload.ExecutorKind = strings.TrimSpace(payload.ExecutorKind)
	payload.DispatchKey = strings.TrimSpace(payload.DispatchKey)
	payload.AttemptID = strings.TrimSpace(payload.AttemptID)
	payload.ProviderRunID = strings.TrimSpace(payload.ProviderRunID)
	payload.ObserverNotificationChannel = strings.TrimSpace(payload.ObserverNotificationChannel)
	payload.ObserverNotificationID = strings.TrimSpace(payload.ObserverNotificationID)
	payload.ObserverDispatchToken = strings.TrimSpace(payload.ObserverDispatchToken)
	return payload
}

func validateRequirementSyncPayload(payload RequirementSyncPayload) (pgtype.UUID, error) {
	if payload.ID == "" {
		return pgtype.UUID{}, ErrMissingRecordID
	}
	if payload.Title == "" {
		return pgtype.UUID{}, ErrMissingTitle
	}
	if strings.TrimSpace(payload.ExecutionPrompt) == "" {
		return pgtype.UUID{}, ErrMissingExecutionPrompt
	}
	if payload.ExecutorID == "" {
		return pgtype.UUID{}, ErrMissingExecutorID
	}
	executorID, err := util.ParseUUID(payload.ExecutorID)
	if err != nil {
		return pgtype.UUID{}, ErrInvalidExecutorID
	}
	if payload.OwnerID == "" {
		return pgtype.UUID{}, ErrMissingRequirementOwnerID
	}
	if payload.ProductIterationID <= 0 {
		return pgtype.UUID{}, ErrMissingProductIterationID
	}
	if payload.ProjectRecordURL == "" {
		return pgtype.UUID{}, ErrMissingProjectRecordURL
	}
	return executorID, nil
}

func buildRequirementIssueContent(payload RequirementSyncPayload) requirementIssueContent {
	attempt := decodeRequirementCurrentAttempt(payload.CurrentAttempt)
	attemptID := firstRequirementValue(payload.AttemptID, attempt.AttemptID, attempt.ID)
	if attemptID == "" && strings.HasPrefix(payload.ObserverDispatchToken, "att_") {
		attemptID = payload.ObserverDispatchToken
	}
	dispatchKey := firstRequirementValue(payload.DispatchKey, attempt.DispatchKey)
	if dispatchKey == "" && strings.HasPrefix(payload.ObserverDispatchToken, "syndra-flow-v1:") {
		dispatchKey = payload.ObserverDispatchToken
	}
	if dispatchKey == "" {
		dispatchKey = "syndra-flow-v1:" + payload.ID
		if attemptID != "" {
			dispatchKey += ":" + attemptID
		}
	}
	providerRunID := firstRequirementValue(payload.ProviderRunID, attempt.ProviderRunID)
	title := fmt.Sprintf("[SYN:v1:%s] %s", dispatchKey, payload.Title)
	description := fmt.Sprintf(
		"结构化需求源：\nexternal.product_iteration_id=%d\nproject_record_url=%s\ndevelopment_role=%s\n\n%s\n\n---\n\n[SYN:v1:%s]\ndispatch_key=%s\nlane_id=%s\nwork_unit_id=%s\nexternal.lane_type=%s\nexternal.provider_run_id=%s",
		payload.ProductIterationID,
		payload.ProjectRecordURL,
		payload.DevelopmentRole,
		strings.TrimRight(payload.ExecutionPrompt, "\r\n"),
		dispatchKey,
		dispatchKey,
		payload.LaneID,
		payload.ID,
		payload.LaneType,
		providerRunID,
	)
	return requirementIssueContent{
		Title:         title,
		Description:   description,
		DispatchKey:   dispatchKey,
		AttemptID:     attemptID,
		ProviderRunID: providerRunID,
	}
}

func decodeRequirementCurrentAttempt(raw json.RawMessage) requirementCurrentAttempt {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return requirementCurrentAttempt{}
	}
	if strings.HasPrefix(value, "\"") {
		var id string
		if json.Unmarshal(raw, &id) == nil {
			return requirementCurrentAttempt{ID: strings.TrimSpace(id)}
		}
		return requirementCurrentAttempt{}
	}
	var attempt requirementCurrentAttempt
	if json.Unmarshal(raw, &attempt) != nil {
		return requirementCurrentAttempt{}
	}
	attempt.ID = strings.TrimSpace(attempt.ID)
	attempt.AttemptID = strings.TrimSpace(attempt.AttemptID)
	attempt.DispatchKey = strings.TrimSpace(attempt.DispatchKey)
	attempt.ProviderRunID = strings.TrimSpace(attempt.ProviderRunID)
	return attempt
}

func firstRequirementValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func requirementIssueMetadata(payload RequirementSyncPayload, content requirementIssueContent) map[string]any {
	metadata := map[string]any{
		"provider":              defaultRequirementProvider,
		"source_record_id":      payload.ID,
		"syndra_requirement_id": payload.ProductIterationID,
		"project_record_url":    payload.ProjectRecordURL,
		"syndra_owner_id":       payload.OwnerID,
		"syndra_executor_id":    payload.ExecutorID,
		"dispatch_key":          content.DispatchKey,
	}
	addRequirementMetadataString(metadata, "size", payload.Size)
	addRequirementMetadataString(metadata, "development_role", payload.DevelopmentRole)
	addRequirementMetadataString(metadata, "syndra_state", payload.State)
	addRequirementMetadataString(metadata, "lane_id", payload.LaneID)
	addRequirementMetadataString(metadata, "lane_type", payload.LaneType)
	addRequirementMetadataString(metadata, "executor_kind", payload.ExecutorKind)
	addRequirementMetadataString(metadata, "attempt_id", content.AttemptID)
	addRequirementMetadataString(metadata, "provider_run_id", content.ProviderRunID)
	addRequirementMetadataString(metadata, "observer_notification_channel", payload.ObserverNotificationChannel)
	addRequirementMetadataString(metadata, "observer_notification_id", payload.ObserverNotificationID)
	return metadata
}

func addRequirementMetadataString(metadata map[string]any, key, value string) {
	if value != "" {
		metadata[key] = value
	}
}

func (i *Importer) resolveRequirementExecutor(ctx context.Context, workspaceID, executorID pgtype.UUID, payload RequirementSyncPayload) (db.Agent, error) {
	i.info("external requirement sync: executor resolution started",
		"provider", defaultRequirementProvider,
		"workspace_id", util.UUIDToString(workspaceID),
		"source_record_id", payload.ID,
		"executor_id", util.UUIDToString(executorID),
		"syndra_owner_id", payload.OwnerID,
	)
	agent, err := i.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
		ID:          executorID,
		WorkspaceID: workspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		i.warn("external requirement sync: executor not found in workspace",
			"provider", defaultRequirementProvider,
			"workspace_id", util.UUIDToString(workspaceID),
			"source_record_id", payload.ID,
			"executor_id", util.UUIDToString(executorID),
		)
		return db.Agent{}, ErrRequirementExecutorNotFound
	}
	if err != nil {
		return db.Agent{}, fmt.Errorf("load requirement executor agent: %w", err)
	}
	if !agent.OwnerID.Valid {
		i.warn("external requirement sync: executor has no owner",
			"provider", defaultRequirementProvider,
			"workspace_id", util.UUIDToString(workspaceID),
			"source_record_id", payload.ID,
			"executor_id", util.UUIDToString(executorID),
		)
		return db.Agent{}, ErrRequirementOwnerNotMember
	}
	if _, err := i.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      agent.OwnerID,
		WorkspaceID: workspaceID,
	}); errors.Is(err, pgx.ErrNoRows) {
		i.warn("external requirement sync: executor owner is not a workspace member",
			"provider", defaultRequirementProvider,
			"workspace_id", util.UUIDToString(workspaceID),
			"source_record_id", payload.ID,
			"executor_id", util.UUIDToString(executorID),
			"creator_id", util.UUIDToString(agent.OwnerID),
		)
		return db.Agent{}, ErrRequirementOwnerNotMember
	} else if err != nil {
		return db.Agent{}, fmt.Errorf("load requirement executor owner membership: %w", err)
	}
	ready, reason, err := service.AgentReadiness(ctx, i.Queries, agent)
	if err != nil {
		return db.Agent{}, fmt.Errorf("check requirement executor readiness: %w", err)
	}
	if !ready {
		i.warn("external requirement sync: executor is not ready",
			"provider", defaultRequirementProvider,
			"workspace_id", util.UUIDToString(workspaceID),
			"source_record_id", payload.ID,
			"executor_id", util.UUIDToString(executorID),
			"creator_id", util.UUIDToString(agent.OwnerID),
			"reason", reason,
		)
		return db.Agent{}, fmt.Errorf("%w: %s", ErrRequirementAgentNotReady, reason)
	}
	i.info("external requirement sync: executor resolved",
		"provider", defaultRequirementProvider,
		"workspace_id", util.UUIDToString(workspaceID),
		"source_record_id", payload.ID,
		"executor_id", util.UUIDToString(agent.ID),
		"creator_id", util.UUIDToString(agent.OwnerID),
		"runtime_id", util.UUIDToString(agent.RuntimeID),
	)
	return agent, nil
}

func (i *Importer) updateExistingRequirementIssue(ctx context.Context, existing db.Issue, payload RequirementSyncPayload, content requirementIssueContent, executorID pgtype.UUID, metadata []byte) (RequirementSyncResult, error) {
	if !existing.AssigneeType.Valid || existing.AssigneeType.String != "agent" || !existing.AssigneeID.Valid || existing.AssigneeID != executorID {
		i.warn("external requirement sync: idempotent executor conflict",
			"provider", defaultRequirementProvider,
			"workspace_id", util.UUIDToString(existing.WorkspaceID),
			"source_record_id", payload.ID,
			"issue_id", util.UUIDToString(existing.ID),
			"current_assignee_type", existing.AssigneeType.String,
			"current_assignee_id", util.UUIDToString(existing.AssigneeID),
			"incoming_executor_id", util.UUIDToString(executorID),
		)
		return RequirementSyncResult{}, ErrRequirementExecutorConflict
	}
	i.info("external requirement sync: existing issue matched",
		"provider", defaultRequirementProvider,
		"workspace_id", util.UUIDToString(existing.WorkspaceID),
		"source_record_id", payload.ID,
		"issue_id", util.UUIDToString(existing.ID),
		"issue_number", existing.Number,
		"executor_id", util.UUIDToString(executorID),
	)
	updated, err := i.stampRequirementIssue(ctx, existing, payload, content, metadata, true)
	if err != nil {
		return RequirementSyncResult{}, err
	}
	if err := i.ensureRequirementTask(ctx, updated, executorID, payload.ID); err != nil {
		return RequirementSyncResult{}, err
	}
	i.info("external requirement sync: completed",
		"provider", defaultRequirementProvider,
		"workspace_id", util.UUIDToString(updated.WorkspaceID),
		"source_record_id", payload.ID,
		"issue_id", util.UUIDToString(updated.ID),
		"existing", true,
	)
	return newRequirementSyncResult(updated, payload, true), nil
}

func (i *Importer) stampRequirementIssue(ctx context.Context, issue db.Issue, payload RequirementSyncPayload, content requirementIssueContent, metadata []byte, publishUpdate bool) (db.Issue, error) {
	updated, err := i.Queries.UpdateIssueFromExternalSync(ctx, db.UpdateIssueFromExternalSyncParams{
		Title:       content.Title,
		Description: util.StrToText(content.Description),
		Status:      issue.Status,
		Priority:    issue.Priority,
		Metadata:    metadata,
		ID:          issue.ID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		i.warn("external requirement sync: issue source fields update failed",
			"provider", defaultRequirementProvider,
			"workspace_id", util.UUIDToString(issue.WorkspaceID),
			"source_record_id", payload.ID,
			"issue_id", util.UUIDToString(issue.ID),
			"error", err,
		)
		return db.Issue{}, fmt.Errorf("update Syndra requirement issue: %w", err)
	}
	i.info("external requirement sync: issue source fields updated",
		"provider", defaultRequirementProvider,
		"workspace_id", util.UUIDToString(issue.WorkspaceID),
		"source_record_id", payload.ID,
		"issue_id", util.UUIDToString(updated.ID),
		"issue_number", updated.Number,
		"status", updated.Status,
		"title_changed", issue.Title != updated.Title,
		"description_changed", textString(issue.Description) != textString(updated.Description),
	)
	if publishUpdate {
		i.publishExternalIssueUpdated(ctx, issue, updated, "external_requirement_sync")
	}
	i.publishIssueMetadataChanged(updated, "external_requirement_sync")
	return updated, nil
}

func (i *Importer) ensureRequirementTask(ctx context.Context, issue db.Issue, executorID pgtype.UUID, sourceRecordID string) error {
	tasks, err := i.Queries.ListTasksByIssue(ctx, issue.ID)
	if err != nil {
		return fmt.Errorf("list automatic development tasks for requirement issue: %w", err)
	}
	for _, task := range tasks {
		if task.AgentID == executorID {
			i.info("external requirement sync: automatic development task confirmed",
				"provider", defaultRequirementProvider,
				"workspace_id", util.UUIDToString(issue.WorkspaceID),
				"source_record_id", sourceRecordID,
				"issue_id", util.UUIDToString(issue.ID),
				"executor_id", util.UUIDToString(executorID),
				"task_id", util.UUIDToString(task.ID),
				"task_status", task.Status,
			)
			return nil
		}
	}
	if i.IssueService.TaskService == nil {
		return errors.New("automatic development task service is not wired")
	}
	i.warn("external requirement sync: automatic development task missing, retrying enqueue",
		"provider", defaultRequirementProvider,
		"workspace_id", util.UUIDToString(issue.WorkspaceID),
		"source_record_id", sourceRecordID,
		"issue_id", util.UUIDToString(issue.ID),
		"executor_id", util.UUIDToString(executorID),
	)
	task, err := i.IssueService.TaskService.EnqueueTaskForIssue(ctx, issue)
	if err != nil {
		i.warn("external requirement sync: automatic development task enqueue failed",
			"provider", defaultRequirementProvider,
			"workspace_id", util.UUIDToString(issue.WorkspaceID),
			"source_record_id", sourceRecordID,
			"issue_id", util.UUIDToString(issue.ID),
			"executor_id", util.UUIDToString(executorID),
			"error", err,
		)
		return fmt.Errorf("enqueue automatic development task for requirement issue: %w", err)
	}
	i.info("external requirement sync: automatic development task enqueued",
		"provider", defaultRequirementProvider,
		"workspace_id", util.UUIDToString(issue.WorkspaceID),
		"source_record_id", sourceRecordID,
		"issue_id", util.UUIDToString(issue.ID),
		"executor_id", util.UUIDToString(executorID),
		"task_id", util.UUIDToString(task.ID),
		"task_status", task.Status,
	)
	return nil
}

func newRequirementSyncResult(issue db.Issue, payload RequirementSyncPayload, existing bool) RequirementSyncResult {
	return RequirementSyncResult{
		Issue:              issue,
		Existing:           existing,
		Provider:           defaultRequirementProvider,
		SourceRecordID:     payload.ID,
		ProductIterationID: payload.ProductIterationID,
		ProjectRecordURL:   payload.ProjectRecordURL,
	}
}
