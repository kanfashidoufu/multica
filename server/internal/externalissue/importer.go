package externalissue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	htmlstd "html"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	enterpriseLark "github.com/multica-ai/multica/server/internal/enterprise/lark"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/storage"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	OriginType              = "external_issue"
	defaultTargetType       = "小需求"
	defaultStatus           = "backlog"
	defaultPriority         = "none"
	defaultProvider         = "lark_base"
	defaultBugProvider      = "syndra"
	defaultBugStatus        = "todo"
	defaultAttachmentLimit  = 20
	defaultAttachmentMax    = 100 << 20
	defaultDownloadTimeout  = 30 * time.Second
	defaultOpenAPITimeout   = 30 * time.Second
	defaultDownloadFileName = "attachment"
	tokenSafetyMargin       = 60 * time.Second
	maxBugMetadataKeys      = 50
	maxBugMetadataStringLen = 1000
)

var (
	ErrUnauthorized             = errors.New("external issue import token is invalid")
	ErrNotConfigured            = errors.New("external issue import is not configured")
	ErrMissingWorkspaceID       = errors.New("workspace_id is required")
	ErrMissingDefaultAssignee   = errors.New("default assignee external user id is not configured")
	ErrDefaultAssigneeNotMember = errors.New("default assignee is not a member of this workspace")
	ErrMissingRecordID          = errors.New("external record id is required")
	ErrMissingTitle             = errors.New("external issue title is required")
	ErrStorageNotConfigured     = errors.New("attachment storage is not configured")
	ErrMissingLarkRecordParams  = errors.New("app_token, table_id, and record_id are required when fields are omitted")
	ErrLarkAppNotConfigured     = errors.New("Lark app credentials are not configured")
	ErrLarkOpenAPITimeout       = errors.New("Lark OpenAPI request timed out")
)

var (
	bugBreakTagRE     = regexp.MustCompile(`(?i)<\s*br\s*/?\s*>|</\s*(p|div|section|li|tr)\s*>`)
	bugBlockOpenTagRE = regexp.MustCompile(`(?i)<\s*(p|div|section|tr)\b[^>]*>`)
	bugListOpenTagRE  = regexp.MustCompile(`(?i)<\s*li\b[^>]*>`)
	bugAnyTagRE       = regexp.MustCompile(`(?s)<[^>]*>`)
)

type Config struct {
	WebhookToken                  string
	DefaultWorkspaceID            string
	DefaultAssigneeExternalUserID string
	LarkAppID                     string
	LarkAppSecret                 string
	LarkOpenAPIBaseURL            string
	AttachmentDownloadTimeout     time.Duration
	LarkOpenAPITimeout            time.Duration
	AttachmentMaxBytes            int64
	AttachmentLimit               int
}

func (c Config) withDefaults() Config {
	c.WebhookToken = strings.TrimSpace(c.WebhookToken)
	c.DefaultWorkspaceID = strings.TrimSpace(c.DefaultWorkspaceID)
	c.DefaultAssigneeExternalUserID = strings.TrimSpace(c.DefaultAssigneeExternalUserID)
	c.LarkAppID = strings.TrimSpace(c.LarkAppID)
	c.LarkAppSecret = strings.TrimSpace(c.LarkAppSecret)
	c.LarkOpenAPIBaseURL = strings.TrimRight(strings.TrimSpace(c.LarkOpenAPIBaseURL), "/")
	if c.LarkOpenAPIBaseURL == "" {
		c.LarkOpenAPIBaseURL = lark.RegionFeishu.OpenPlatformBaseURL()
	}
	if c.AttachmentDownloadTimeout <= 0 {
		c.AttachmentDownloadTimeout = defaultDownloadTimeout
	}
	if c.LarkOpenAPITimeout <= 0 {
		c.LarkOpenAPITimeout = defaultOpenAPITimeout
	}
	if c.AttachmentMaxBytes <= 0 {
		c.AttachmentMaxBytes = defaultAttachmentMax
	}
	if c.AttachmentLimit <= 0 {
		c.AttachmentLimit = defaultAttachmentLimit
	}
	return c
}

type Importer struct {
	Queries           *db.Queries
	IssueService      *service.IssueService
	Storage           storage.Storage
	LarkInstallations *lark.InstallationService
	LarkAPIClient     lark.APIClient
	Bus               *events.Bus
	BroadcastPayload  func(ctx context.Context, issue db.Issue, attachments []db.Attachment) map[string]any
	HTTPClient        *http.Client
	Logger            *slog.Logger
	Config            Config

	larkAppTokenMu sync.Mutex
	larkAppToken   cachedLarkAppToken
}

type Request struct {
	Provider       string          `json:"provider,omitempty"`
	Source         string          `json:"source,omitempty"`
	WorkspaceID    string          `json:"workspace_id,omitempty"`
	InstallationID string          `json:"installation_id,omitempty"`
	AppToken       string          `json:"app_token,omitempty"`
	BaseToken      string          `json:"base_token,omitempty"`
	TableID        string          `json:"table_id,omitempty"`
	ViewID         string          `json:"view_id,omitempty"`
	RecordID       string          `json:"record_id,omitempty"`
	RecordURL      string          `json:"record_url,omitempty"`
	Fields         map[string]any  `json:"fields,omitempty"`
	Record         json.RawMessage `json:"record,omitempty"`
	FieldMapping   FieldMapping    `json:"field_mapping,omitempty"`
	TargetType     string          `json:"target_type,omitempty"`
	AssigneeUserID string          `json:"assignee_user_id,omitempty"`
	AllowDuplicate bool            `json:"allow_duplicate,omitempty"`
}

type BugSyncRequest struct {
	WorkspaceID    string
	AssigneeUserID string
	Payload        BugSyncPayload
}

type BugSyncPayload struct {
	SchemaVersion string        `json:"schema_version,omitempty"`
	EventType     string        `json:"event_type,omitempty"`
	EventID       string        `json:"event_id,omitempty"`
	Scene         string        `json:"scene,omitempty"`
	Source        string        `json:"source,omitempty"`
	SourceEnv     string        `json:"source_env,omitempty"`
	SentAt        string        `json:"sent_at,omitempty"`
	ItemCount     int           `json:"item_count,omitempty"`
	ItemIDs       string        `json:"item_ids,omitempty"`
	Items         []BugSyncItem `json:"items,omitempty"`
}

type BugSyncItem struct {
	Event               string         `json:"event,omitempty"`
	EntityType          string         `json:"entity_type,omitempty"`
	ExternalKey         string         `json:"external_key,omitempty"`
	BugID               int64          `json:"bug_id,omitempty"`
	VersionID           int64          `json:"version_id,omitempty"`
	VersionName         string         `json:"version_name,omitempty"`
	DemandName          string         `json:"demand_name,omitempty"`
	Role                string         `json:"role,omitempty"`
	Title               string         `json:"title,omitempty"`
	Description         string         `json:"description,omitempty"`
	Priority            string         `json:"priority,omitempty"`
	BugLevel            string         `json:"bug_level,omitempty"`
	BugTypeID           int64          `json:"bug_type_id,omitempty"`
	BugType             string         `json:"bug_type,omitempty"`
	Status              string         `json:"status,omitempty"`
	StatusName          string         `json:"status_name,omitempty"`
	ResolveSolution     any            `json:"resolve_solution,omitempty"`
	ResolveSolutionName string         `json:"resolve_solution_name,omitempty"`
	Owner               BugSyncPerson  `json:"owner,omitempty"`
	Creator             BugSyncPerson  `json:"creator,omitempty"`
	Assignee            BugSyncPerson  `json:"assignee,omitempty"`
	Solver              BugSyncPerson  `json:"solver,omitempty"`
	Module              BugSyncModule  `json:"module,omitempty"`
	DuplicateBug        any            `json:"duplicate_bug,omitempty"`
	Attachments         []BugSyncMedia `json:"attachments,omitempty"`
	Videos              []BugSyncMedia `json:"videos,omitempty"`
	BugDetail           BugSyncDetail  `json:"bug_detail,omitempty"`
	Labels              []string       `json:"labels,omitempty"`
	SourceURL           string         `json:"source_url,omitempty"`
	CreatedAt           string         `json:"created_at,omitempty"`
	UpdatedAt           string         `json:"updated_at,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
}

type BugSyncPerson struct {
	MateID   *int64  `json:"mate_id,omitempty"`
	Name     *string `json:"name,omitempty"`
	DeptName *string `json:"dept_name,omitempty"`
}

type BugSyncModule struct {
	ModuleID   int64  `json:"module_id,omitempty"`
	ModuleName string `json:"module_name,omitempty"`
}

type BugSyncMedia struct {
	Name      string `json:"name,omitempty"`
	URL       string `json:"url,omitempty"`
	FileURL   string `json:"file_url,omitempty"`
	FileToken string `json:"file_token,omitempty"`
}

type BugSyncDetail struct {
	BugID               int64          `json:"bug_id,omitempty"`
	Title               string         `json:"title,omitempty"`
	Description         string         `json:"description,omitempty"`
	BugLevel            string         `json:"bug_level,omitempty"`
	Priority            string         `json:"priority,omitempty"`
	BugTypeID           int64          `json:"bug_type_id,omitempty"`
	BugTypeName         string         `json:"bug_type_name,omitempty"`
	Status              string         `json:"status,omitempty"`
	StatusName          string         `json:"status_name,omitempty"`
	ResolveSolution     any            `json:"resolve_solution,omitempty"`
	ResolveSolutionName string         `json:"resolve_solution_name,omitempty"`
	DuplicateBug        any            `json:"duplicate_bug,omitempty"`
	Module              BugSyncModule  `json:"module,omitempty"`
	Version             BugSyncVersion `json:"version,omitempty"`
	Creator             BugSyncPerson  `json:"creator,omitempty"`
	Assignee            BugSyncPerson  `json:"assignee,omitempty"`
	Solver              BugSyncPerson  `json:"solver,omitempty"`
	Deadline            string         `json:"deadline,omitempty"`
	BugSolveTime        string         `json:"bug_solve_time,omitempty"`
	ReopenCount         int64          `json:"reopen_count,omitempty"`
	LowBug              int64          `json:"low_bug,omitempty"`
	IsBug               int64          `json:"is_bug,omitempty"`
	CaseID              int64          `json:"case_id,omitempty"`
	BugURL              string         `json:"bug_url,omitempty"`
	SourceURL           string         `json:"source_url,omitempty"`
	Attachments         []BugSyncMedia `json:"attachments,omitempty"`
	Videos              []BugSyncMedia `json:"videos,omitempty"`
	CreatedAt           string         `json:"created_at,omitempty"`
	UpdatedAt           string         `json:"updated_at,omitempty"`
}

type BugSyncVersion struct {
	VersionID     int64  `json:"version_id,omitempty"`
	VersionName   string `json:"version_name,omitempty"`
	DemandName    string `json:"demand_name,omitempty"`
	VersionType   int64  `json:"version_type,omitempty"`
	VersionStatus int64  `json:"version_status,omitempty"`
}

type FieldMapping struct {
	VersionType string `json:"version_type,omitempty"`
	Version     string `json:"version,omitempty"`
	Name        string `json:"name,omitempty"`
	Notes       string `json:"notes,omitempty"`
	Attachments string `json:"attachments,omitempty"`
}

type Result struct {
	Ignored          bool
	Reason           string
	Issue            db.Issue
	Existing         bool
	Attachments      []db.Attachment
	AttachmentErrors []AttachmentError
	Provider         string
	SourceRecordID   string
}

type BugSyncResult struct {
	Provider string
	Items    []BugSyncItemResult
}

type BugSyncItemResult struct {
	Ignored        bool
	Reason         string
	Issue          db.Issue
	Existing       bool
	Provider       string
	SourceRecordID string
	ExternalKey    string
	BugID          int64
}

type AttachmentError struct {
	Name  string `json:"name,omitempty"`
	Token string `json:"token,omitempty"`
	URL   string `json:"url,omitempty"`
	Error string `json:"error"`
}

type AttachmentSource struct {
	Name        string
	URL         string
	TmpURL      string
	FileToken   string
	ContentType string
	SizeBytes   int64
}

type cachedLarkAppToken struct {
	AppID     string
	BaseURL   string
	Value     string
	ExpiresAt time.Time
}

type normalizedRecord struct {
	Provider       string
	WorkspaceID    pgtype.UUID
	InstallationID pgtype.UUID
	BaseToken      string
	TableID        string
	ViewID         string
	RecordID       string
	RecordURL      string
	VersionType    string
	Title          string
	Name           string
	Notes          string
	Description    string
	AssigneeUserID string
	Attachments    []AttachmentSource
	RawFields      map[string]any
}

func (i *Importer) Import(ctx context.Context, req Request) (Result, error) {
	cfg := i.Config.withDefaults()
	if cfg.WebhookToken == "" {
		return Result{}, ErrNotConfigured
	}
	if i.Queries == nil || i.IssueService == nil {
		return Result{}, errors.New("external issue importer is not wired")
	}
	req = req.withDefaults(cfg)
	if req.WorkspaceID == "" {
		return Result{}, ErrMissingWorkspaceID
	}
	if err := i.hydrateFields(ctx, &req); err != nil {
		return Result{}, err
	}

	rec, err := i.normalize(req)
	if err != nil {
		return Result{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(rec.VersionType), targetType(req.TargetType)) {
		return Result{
			Ignored:        true,
			Reason:         "version_type_not_target",
			Provider:       rec.Provider,
			SourceRecordID: rec.RecordID,
		}, nil
	}

	defaultUserID := firstNonEmpty(strings.TrimSpace(req.AssigneeUserID), cfg.DefaultAssigneeExternalUserID)
	if defaultUserID == "" {
		return Result{}, ErrMissingDefaultAssignee
	}
	assignee, err := i.resolveMemberByExternalUserID(ctx, rec.WorkspaceID, defaultUserID)
	if err != nil {
		return Result{}, err
	}

	originID := sourceOriginID(rec.Provider, rec.BaseToken, rec.TableID, rec.RecordID)
	existing, err := i.Queries.GetIssueByOrigin(ctx, db.GetIssueByOriginParams{
		WorkspaceID: rec.WorkspaceID,
		OriginType:  pgtype.Text{String: OriginType, Valid: true},
		OriginID:    originID,
	})
	if err == nil {
		attachments, _ := i.Queries.ListAttachmentsByIssue(ctx, db.ListAttachmentsByIssueParams{
			IssueID:     existing.ID,
			WorkspaceID: existing.WorkspaceID,
		})
		return Result{
			Issue:          existing,
			Existing:       true,
			Attachments:    attachments,
			Provider:       rec.Provider,
			SourceRecordID: rec.RecordID,
		}, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, fmt.Errorf("lookup existing issue by origin: %w", err)
	}

	attachmentIDs, attachmentRows, attachmentErrors := i.createAttachments(ctx, rec, assignee.UserID)
	description := appendAttachmentMarkdown(rec.Description, attachmentRows)
	createOpts := service.IssueCreateOpts{
		ActorID:  util.UUIDToString(assignee.UserID),
		Platform: "external_import:" + rec.Provider,
	}
	if i.BroadcastPayload != nil {
		createOpts.BroadcastPayload = func(issue db.Issue, attachments []db.Attachment) map[string]any {
			return i.BroadcastPayload(ctx, issue, attachments)
		}
	}

	res, err := i.IssueService.Create(ctx, service.IssueCreateParams{
		WorkspaceID:    rec.WorkspaceID,
		Title:          rec.Title,
		Description:    util.StrToText(description),
		Status:         defaultStatus,
		Priority:       defaultPriority,
		AssigneeType:   pgtype.Text{String: "member", Valid: true},
		AssigneeID:     assignee.UserID,
		CreatorType:    "member",
		CreatorID:      assignee.UserID,
		OriginType:     pgtype.Text{String: OriginType, Valid: true},
		OriginID:       originID,
		AttachmentIDs:  attachmentIDs,
		AllowDuplicate: true,
	}, createOpts)
	if isUniqueViolation(err, "idx_issue_external_origin_unique") {
		existing, lookupErr := i.Queries.GetIssueByOrigin(ctx, db.GetIssueByOriginParams{
			WorkspaceID: rec.WorkspaceID,
			OriginType:  pgtype.Text{String: OriginType, Valid: true},
			OriginID:    originID,
		})
		if lookupErr == nil {
			i.deleteUnlinkedAttachments(ctx, rec.WorkspaceID, attachmentIDs)
			attachments, _ := i.Queries.ListAttachmentsByIssue(ctx, db.ListAttachmentsByIssueParams{
				IssueID:     existing.ID,
				WorkspaceID: existing.WorkspaceID,
			})
			return Result{
				Issue:            existing,
				Existing:         true,
				Attachments:      attachments,
				AttachmentErrors: attachmentErrors,
				Provider:         rec.Provider,
				SourceRecordID:   rec.RecordID,
			}, nil
		}
	}
	if err != nil {
		i.deleteUnlinkedAttachments(ctx, rec.WorkspaceID, attachmentIDs)
		return Result{}, err
	}

	attachments := res.Attachments
	if len(attachments) == 0 && len(attachmentRows) > 0 {
		attachments = attachmentRows
	}
	i.notifyAssignee(ctx, res.Issue, assignee.UserID)
	return Result{
		Issue:            res.Issue,
		Attachments:      attachments,
		AttachmentErrors: attachmentErrors,
		Provider:         rec.Provider,
		SourceRecordID:   rec.RecordID,
	}, nil
}

func (i *Importer) ImportBugSync(ctx context.Context, req BugSyncRequest) (BugSyncResult, error) {
	cfg := i.Config.withDefaults()
	if cfg.WebhookToken == "" {
		return BugSyncResult{}, ErrNotConfigured
	}
	if i.Queries == nil || i.IssueService == nil {
		return BugSyncResult{}, errors.New("external issue importer is not wired")
	}

	workspaceIDRaw := firstNonEmpty(req.WorkspaceID, cfg.DefaultWorkspaceID)
	if workspaceIDRaw == "" {
		return BugSyncResult{}, ErrMissingWorkspaceID
	}
	workspaceID, err := util.ParseUUID(workspaceIDRaw)
	if err != nil {
		return BugSyncResult{}, ErrMissingWorkspaceID
	}

	defaultUserID := firstNonEmpty(strings.TrimSpace(req.AssigneeUserID), cfg.DefaultAssigneeExternalUserID)
	if defaultUserID == "" {
		return BugSyncResult{}, ErrMissingDefaultAssignee
	}
	defaultAssignee, err := i.resolveMemberByExternalUserID(ctx, workspaceID, defaultUserID)
	if err != nil {
		return BugSyncResult{}, err
	}

	provider := bugProvider(req.Payload)
	result := BugSyncResult{Provider: provider}
	for _, item := range req.Payload.Items {
		assignee, err := i.resolveBugAssigneeMember(ctx, workspaceID, item, defaultAssignee)
		if err != nil {
			return result, err
		}
		itemResult, err := i.importBugSyncItem(ctx, req.Payload, item, workspaceID, assignee.UserID)
		if err != nil {
			return result, err
		}
		result.Items = append(result.Items, itemResult)
	}
	return result, nil
}

func (i *Importer) importBugSyncItem(ctx context.Context, payload BugSyncPayload, item BugSyncItem, workspaceID pgtype.UUID, assigneeID pgtype.UUID) (BugSyncItemResult, error) {
	provider := bugProvider(payload)
	recordID := bugRecordID(item)
	if recordID == "" {
		return BugSyncItemResult{}, ErrMissingRecordID
	}
	if !bugSyncEventSupported(item.Event) {
		return BugSyncItemResult{
			Ignored:        true,
			Reason:         "unsupported_event",
			Provider:       provider,
			SourceRecordID: recordID,
			ExternalKey:    item.ExternalKey,
			BugID:          item.BugID,
		}, nil
	}

	originID := sourceOriginID(provider, payload.SourceEnv, firstNonEmpty(item.EntityType, "version_bug"), recordID)
	title := bugIssueTitle(item)
	if title == "" {
		return BugSyncItemResult{}, ErrMissingTitle
	}
	description := bugIssueDescription(item)
	status := bugIssueStatus(item)
	priority := bugIssuePriority(item)
	metadata := bugIssueMetadata(payload, item, provider, recordID)
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return BugSyncItemResult{}, err
	}

	existing, err := i.Queries.GetIssueByOrigin(ctx, db.GetIssueByOriginParams{
		WorkspaceID: workspaceID,
		OriginType:  pgtype.Text{String: OriginType, Valid: true},
		OriginID:    originID,
	})
	if err == nil {
		updated, err := i.Queries.UpdateIssueFromExternalSync(ctx, db.UpdateIssueFromExternalSyncParams{
			Title:       title,
			Description: util.StrToText(description),
			Status:      status,
			Priority:    priority,
			Metadata:    metadataBytes,
			ID:          existing.ID,
			WorkspaceID: existing.WorkspaceID,
		})
		if err != nil {
			return BugSyncItemResult{}, fmt.Errorf("update external bug issue: %w", err)
		}
		i.publishExternalIssueUpdated(ctx, existing, updated, "external_bug_sync")
		i.publishIssueMetadataChanged(updated, "external_bug_sync")
		return BugSyncItemResult{
			Issue:          updated,
			Existing:       true,
			Provider:       provider,
			SourceRecordID: recordID,
			ExternalKey:    item.ExternalKey,
			BugID:          item.BugID,
		}, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return BugSyncItemResult{}, fmt.Errorf("lookup existing bug issue by origin: %w", err)
	}

	createOpts := service.IssueCreateOpts{
		ActorID:  util.UUIDToString(assigneeID),
		Platform: "external_import:" + provider,
	}
	if i.BroadcastPayload != nil {
		createOpts.BroadcastPayload = func(issue db.Issue, attachments []db.Attachment) map[string]any {
			return i.BroadcastPayload(ctx, issue, attachments)
		}
	}
	res, err := i.IssueService.Create(ctx, service.IssueCreateParams{
		WorkspaceID:    workspaceID,
		Title:          title,
		Description:    util.StrToText(description),
		Status:         status,
		Priority:       priority,
		AssigneeType:   pgtype.Text{String: "member", Valid: true},
		AssigneeID:     assigneeID,
		CreatorType:    "member",
		CreatorID:      assigneeID,
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
			updated, updateErr := i.Queries.UpdateIssueFromExternalSync(ctx, db.UpdateIssueFromExternalSyncParams{
				Title:       title,
				Description: util.StrToText(description),
				Status:      status,
				Priority:    priority,
				Metadata:    metadataBytes,
				ID:          existing.ID,
				WorkspaceID: existing.WorkspaceID,
			})
			if updateErr != nil {
				return BugSyncItemResult{}, fmt.Errorf("update raced external bug issue: %w", updateErr)
			}
			i.publishExternalIssueUpdated(ctx, existing, updated, "external_bug_sync")
			i.publishIssueMetadataChanged(updated, "external_bug_sync")
			return BugSyncItemResult{
				Issue:          updated,
				Existing:       true,
				Provider:       provider,
				SourceRecordID: recordID,
				ExternalKey:    item.ExternalKey,
				BugID:          item.BugID,
			}, nil
		}
	}
	if err != nil {
		return BugSyncItemResult{}, err
	}

	updated, err := i.Queries.UpdateIssueFromExternalSync(ctx, db.UpdateIssueFromExternalSyncParams{
		Title:       title,
		Description: util.StrToText(description),
		Status:      status,
		Priority:    priority,
		Metadata:    metadataBytes,
		ID:          res.Issue.ID,
		WorkspaceID: res.Issue.WorkspaceID,
	})
	if err != nil {
		return BugSyncItemResult{}, fmt.Errorf("stamp external bug metadata: %w", err)
	}
	i.publishIssueMetadataChanged(updated, "external_bug_sync")
	i.notifyAssignee(ctx, updated, assigneeID)
	return BugSyncItemResult{
		Issue:          updated,
		Provider:       provider,
		SourceRecordID: recordID,
		ExternalKey:    item.ExternalKey,
		BugID:          item.BugID,
	}, nil
}

func (i *Importer) notifyAssignee(ctx context.Context, issue db.Issue, assigneeID pgtype.UUID) {
	if !assigneeID.Valid {
		return
	}
	if err := i.Queries.AddIssueSubscriber(ctx, db.AddIssueSubscriberParams{
		IssueID:  issue.ID,
		UserType: "member",
		UserID:   assigneeID,
		Reason:   "assignee",
	}); err != nil {
		i.warn("external issue import: subscribe assignee failed",
			"issue_id", util.UUIDToString(issue.ID),
			"assignee_id", util.UUIDToString(assigneeID),
			"error", err,
		)
	} else {
		i.publishSubscriberAdded(issue, assigneeID)
	}

	details, _ := json.Marshal(map[string]any{
		"issue_id": util.UUIDToString(issue.ID),
		"origin":   OriginType,
		"reason":   "assignee",
	})
	item, err := i.Queries.CreateInboxItem(ctx, db.CreateInboxItemParams{
		WorkspaceID:   issue.WorkspaceID,
		RecipientType: "member",
		RecipientID:   assigneeID,
		Type:          "issue_assigned",
		Severity:      "attention",
		IssueID:       issue.ID,
		Title:         issue.Title,
		Body:          pgtype.Text{},
		ActorType:     pgtype.Text{String: "member", Valid: true},
		ActorID:       assigneeID,
		Details:       details,
	})
	if err != nil {
		i.warn("external issue import: assignee inbox write failed",
			"issue_id", util.UUIDToString(issue.ID),
			"assignee_id", util.UUIDToString(assigneeID),
			"error", err,
		)
		return
	}
	i.publishInboxNew(item, issue.Status)
}

func (i *Importer) publishSubscriberAdded(issue db.Issue, assigneeID pgtype.UUID) {
	if i.Bus == nil {
		return
	}
	i.Bus.Publish(events.Event{
		Type:        protocol.EventSubscriberAdded,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   "member",
		ActorID:     util.UUIDToString(assigneeID),
		Payload: map[string]any{
			"issue_id":  util.UUIDToString(issue.ID),
			"user_type": "member",
			"user_id":   util.UUIDToString(assigneeID),
			"reason":    "assignee",
		},
	})
}

func (i *Importer) publishInboxNew(item db.InboxItem, issueStatus string) {
	if i.Bus == nil {
		return
	}
	resp := map[string]any{
		"id":             util.UUIDToString(item.ID),
		"workspace_id":   util.UUIDToString(item.WorkspaceID),
		"recipient_type": item.RecipientType,
		"recipient_id":   util.UUIDToString(item.RecipientID),
		"type":           item.Type,
		"severity":       item.Severity,
		"issue_id":       util.UUIDToPtr(item.IssueID),
		"title":          item.Title,
		"body":           util.TextToPtr(item.Body),
		"read":           item.Read,
		"archived":       item.Archived,
		"created_at":     util.TimestampToString(item.CreatedAt),
		"actor_type":     util.TextToPtr(item.ActorType),
		"actor_id":       util.UUIDToPtr(item.ActorID),
		"details":        json.RawMessage(item.Details),
		"issue_status":   issueStatus,
	}
	actorID := util.UUIDToString(item.ActorID)
	i.Bus.Publish(events.Event{
		Type:        protocol.EventInboxNew,
		WorkspaceID: util.UUIDToString(item.WorkspaceID),
		ActorType:   "member",
		ActorID:     actorID,
		Payload:     map[string]any{"item": resp},
	})
}

func (i *Importer) publishExternalIssueUpdated(ctx context.Context, prev db.Issue, updated db.Issue, source string) {
	if i.Bus == nil {
		return
	}
	issuePayload := any(externalIssueEventPayload(updated))
	if i.BroadcastPayload != nil {
		payload := i.BroadcastPayload(ctx, updated, nil)
		if issue, ok := payload["issue"]; ok {
			issuePayload = issue
		}
	}
	i.Bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: util.UUIDToString(updated.WorkspaceID),
		ActorType:   "system",
		ActorID:     "",
		Payload: map[string]any{
			"issue":               issuePayload,
			"assignee_changed":    false,
			"status_changed":      prev.Status != updated.Status,
			"priority_changed":    prev.Priority != updated.Priority,
			"project_changed":     false,
			"description_changed": textString(prev.Description) != textString(updated.Description),
			"title_changed":       prev.Title != updated.Title,
			"prev_title":          prev.Title,
			"prev_assignee_type":  textString(prev.AssigneeType),
			"prev_assignee_id":    util.UUIDToPtr(prev.AssigneeID),
			"prev_status":         prev.Status,
			"prev_priority":       prev.Priority,
			"prev_description":    util.TextToPtr(prev.Description),
			"creator_type":        prev.CreatorType,
			"creator_id":          util.UUIDToString(prev.CreatorID),
			"source":              source,
		},
	})
}

func (i *Importer) publishIssueMetadataChanged(issue db.Issue, source string) {
	if i.Bus == nil {
		return
	}
	i.Bus.Publish(events.Event{
		Type:        protocol.EventIssueMetadataChanged,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   "system",
		ActorID:     "",
		Payload: map[string]any{
			"issue_id": util.UUIDToString(issue.ID),
			"metadata": externalIssueMetadataMap(issue.Metadata),
			"source":   source,
		},
	})
}

func externalIssueEventPayload(issue db.Issue) map[string]any {
	return map[string]any{
		"id":              util.UUIDToString(issue.ID),
		"workspace_id":    util.UUIDToString(issue.WorkspaceID),
		"number":          issue.Number,
		"title":           issue.Title,
		"description":     util.TextToPtr(issue.Description),
		"status":          issue.Status,
		"priority":        issue.Priority,
		"assignee_type":   util.TextToPtr(issue.AssigneeType),
		"assignee_id":     util.UUIDToPtr(issue.AssigneeID),
		"creator_type":    issue.CreatorType,
		"creator_id":      util.UUIDToString(issue.CreatorID),
		"parent_issue_id": util.UUIDToPtr(issue.ParentIssueID),
		"project_id":      util.UUIDToPtr(issue.ProjectID),
		"position":        issue.Position,
		"metadata":        externalIssueMetadataMap(issue.Metadata),
	}
}

func externalIssueMetadataMap(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func bugProvider(payload BugSyncPayload) string {
	return firstNonEmpty(payload.Source, defaultBugProvider)
}

func bugRecordID(item BugSyncItem) string {
	if v := strings.TrimSpace(item.ExternalKey); v != "" {
		return v
	}
	entityType := firstNonEmpty(item.EntityType, "version_bug")
	if item.BugID != 0 {
		return fmt.Sprintf("%s:%d", entityType, item.BugID)
	}
	return ""
}

func bugSyncEventSupported(event string) bool {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "", "upsert", "create", "created", "update", "updated", "change", "changed":
		return true
	default:
		return false
	}
}

func bugIssueTitle(item BugSyncItem) string {
	base := strings.TrimSpace(item.Title)
	bugID := item.BugID
	if bugID == 0 {
		bugID = item.BugDetail.BugID
	}
	if base == "" && bugID != 0 {
		base = "Syndra Bug"
	}
	if base == "" {
		return ""
	}

	var prefixes []string
	if bugID != 0 {
		prefixes = append(prefixes, fmt.Sprintf("【Bug#%d】", bugID))
	}
	if versionName := strings.TrimSpace(firstNonEmpty(item.VersionName, item.BugDetail.Version.VersionName)); versionName != "" {
		prefixes = append(prefixes, "【"+versionName+"】")
	}
	return strings.Join(prefixes, "") + base
}

func bugIssueDescription(item BugSyncItem) string {
	if description := cleanBugHTMLText(item.Description); description != "" {
		return description
	}
	var parts []string
	if item.VersionName != "" {
		parts = append(parts, "版本："+strings.TrimSpace(item.VersionName))
	}
	if item.BugLevel != "" {
		parts = append(parts, "严重程度："+strings.TrimSpace(item.BugLevel))
	}
	if item.Priority != "" {
		parts = append(parts, "优先级："+strings.TrimSpace(item.Priority))
	}
	if firstNonEmpty(item.StatusName, item.Status) != "" {
		parts = append(parts, "状态："+firstNonEmpty(item.StatusName, item.Status))
	}
	if name := bugPersonName(item.Assignee); name != "" {
		parts = append(parts, "指派人："+name)
	}
	if name := bugPersonName(item.Solver); name != "" {
		parts = append(parts, "解决人："+name)
	}
	if item.SourceURL != "" {
		parts = append(parts, "Syndra："+strings.TrimSpace(item.SourceURL))
	}
	return strings.Join(parts, "\n")
}

func cleanBugHTMLText(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	if !strings.Contains(input, "<") {
		return normalizeBugText(input)
	}
	text := bugBreakTagRE.ReplaceAllString(input, "\n")
	text = bugBlockOpenTagRE.ReplaceAllString(text, "\n")
	text = bugListOpenTagRE.ReplaceAllString(text, "\n- ")
	text = bugAnyTagRE.ReplaceAllString(text, "")
	return normalizeBugText(text)
}

func normalizeBugText(input string) string {
	input = htmlstd.UnescapeString(strings.ReplaceAll(input, "\r\n", "\n"))
	input = strings.ReplaceAll(input, "\r", "\n")
	lines := strings.Split(input, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if len(out) > 0 && !blank {
				out = append(out, "")
				blank = true
			}
			continue
		}
		out = append(out, line)
		blank = false
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func bugIssueStatus(item BugSyncItem) string {
	combined := strings.ToLower(strings.Join([]string{item.Status, item.StatusName}, " "))
	switch {
	case strings.Contains(combined, "cancel") || strings.Contains(combined, "取消"):
		return "cancelled"
	case strings.Contains(combined, "block") || strings.Contains(combined, "阻塞"):
		return "blocked"
	case strings.Contains(combined, "review") || strings.Contains(combined, "验收") || strings.Contains(combined, "待验证"):
		return "in_review"
	case strings.Contains(combined, "progress") || strings.Contains(combined, "处理中") ||
		strings.Contains(combined, "修复中") || strings.Contains(combined, "解决中") ||
		strings.Contains(combined, "进行"):
		return "in_progress"
	case strings.Contains(combined, "resolved") || strings.Contains(combined, "fixed") ||
		strings.Contains(combined, "closed") || strings.Contains(combined, "done") ||
		strings.Contains(combined, "已解决") || strings.Contains(combined, "关闭") ||
		strings.Contains(combined, "完成"):
		return "done"
	case strings.Contains(combined, "active") || strings.Contains(combined, "open") || strings.Contains(combined, "激活"):
		return "todo"
	default:
		return defaultBugStatus
	}
}

func bugIssuePriority(item BugSyncItem) string {
	level := strings.ToUpper(strings.TrimSpace(item.BugLevel))
	switch level {
	case "P0", "P1":
		return "urgent"
	case "P2":
		return "high"
	case "P3":
		return "medium"
	case "P4", "P5":
		return "low"
	}
	priority := strings.ToLower(strings.TrimSpace(item.Priority))
	switch {
	case strings.Contains(priority, "urgent") || strings.Contains(priority, "blocker") || strings.Contains(priority, "紧急"):
		return "urgent"
	case strings.Contains(priority, "high") || strings.Contains(priority, "高"):
		return "high"
	case strings.Contains(priority, "medium") || strings.Contains(priority, "一般") || strings.Contains(priority, "中"):
		return "medium"
	case strings.Contains(priority, "low") || strings.Contains(priority, "低"):
		return "low"
	default:
		return defaultPriority
	}
}

func bugIssueMetadata(payload BugSyncPayload, item BugSyncItem, provider, recordID string) map[string]any {
	b := bugMetadataBuilder{out: map[string]any{}}
	b.add("external_source", provider)
	b.add("external_provider", provider)
	b.add("external_key", recordID)
	b.add("external_url", firstNonEmpty(item.SourceURL, item.BugDetail.SourceURL))
	b.add("sync_schema_version", payload.SchemaVersion)
	b.add("sync_event_type", payload.EventType)
	b.add("sync_event_id", payload.EventID)
	b.add("sync_scene", payload.Scene)
	b.add("sync_source_env", payload.SourceEnv)
	b.add("sync_sent_at", payload.SentAt)
	b.add("bug_id", item.BugID)
	b.add("bug_external_key", item.ExternalKey)
	b.add("bug_entity_type", item.EntityType)
	b.add("bug_event", item.Event)
	b.add("bug_version_id", item.VersionID)
	b.add("bug_version_name", item.VersionName)
	b.add("bug_demand_name", item.DemandName)
	b.add("bug_role", item.Role)
	b.add("bug_level", item.BugLevel)
	b.add("bug_type_id", item.BugTypeID)
	b.add("bug_type", item.BugType)
	b.add("bug_priority", item.Priority)
	b.add("bug_status", item.Status)
	b.add("bug_status_name", item.StatusName)
	b.add("bug_resolve_solution", item.ResolveSolution)
	b.add("bug_resolve_solution_name", item.ResolveSolutionName)
	b.add("bug_created_at", item.CreatedAt)
	b.add("bug_updated_at", item.UpdatedAt)
	b.add("bug_labels", strings.Join(trimNonEmptyStrings(item.Labels), ","))
	b.add("bug_module_id", item.Module.ModuleID)
	b.add("bug_module_name", item.Module.ModuleName)
	b.add("bug_attachment_count", bugMediaCount(item.Attachments, item.BugDetail.Attachments))
	b.add("bug_video_count", bugMediaCount(item.Videos, item.BugDetail.Videos))
	b.add("bug_zentao_url", item.BugDetail.BugURL)
	b.add("bug_deadline", item.BugDetail.Deadline)
	b.add("bug_solve_time", item.BugDetail.BugSolveTime)
	b.add("bug_reopen_count", item.BugDetail.ReopenCount)
	b.add("bug_version_type", item.BugDetail.Version.VersionType)
	b.add("bug_version_status", item.BugDetail.Version.VersionStatus)
	b.addPerson("bug_owner", item.Owner)
	b.addPerson("bug_creator", item.Creator)
	b.addPerson("bug_assignee", item.Assignee)
	b.addPerson("bug_solver", item.Solver)
	for key, value := range item.Metadata {
		b.add(sanitizeBugMetadataKey(key), value)
	}
	return b.out
}

func bugMediaCount(primary, detail []BugSyncMedia) int {
	if len(primary) > 0 {
		return len(primary)
	}
	return len(detail)
}

type bugMetadataBuilder struct {
	out map[string]any
}

func (b *bugMetadataBuilder) addPerson(prefix string, person BugSyncPerson) {
	if person.MateID != nil {
		b.add(prefix+"_mate_id", *person.MateID)
	}
	b.add(prefix+"_name", bugPersonName(person))
	if person.DeptName != nil {
		b.add(prefix+"_dept_name", *person.DeptName)
	}
}

func (b *bugMetadataBuilder) add(key string, value any) {
	if len(b.out) >= maxBugMetadataKeys {
		return
	}
	key = sanitizeBugMetadataKey(key)
	if key == "" {
		return
	}
	value, ok := bugPrimitiveMetadataValue(value)
	if !ok {
		return
	}
	if s, ok := value.(string); ok {
		s = truncateBugMetadataString(strings.TrimSpace(s))
		if s == "" {
			return
		}
		value = s
	}
	b.out[key] = value
}

func bugPrimitiveMetadataValue(value any) (any, bool) {
	switch v := value.(type) {
	case nil:
		return nil, false
	case string:
		return v, true
	case bool:
		return v, true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		if v == 0 {
			return nil, false
		}
		return v, true
	case int64:
		if v == 0 {
			return nil, false
		}
		return v, true
	case int32:
		if v == 0 {
			return nil, false
		}
		return v, true
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i == 0 {
				return nil, false
			}
			return i, true
		}
		if f, err := v.Float64(); err == nil {
			return f, true
		}
		return v.String(), true
	default:
		return nil, false
	}
}

func sanitizeBugMetadataKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			b.WriteRune(r)
		} else if unicode.IsSpace(r) {
			b.WriteByte('_')
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_.-")
	if out == "" {
		return ""
	}
	first := out[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_') {
		out = "external_" + out
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func truncateBugMetadataString(s string) string {
	if len(s) <= maxBugMetadataStringLen {
		return s
	}
	count := 0
	for idx := range s {
		if count == maxBugMetadataStringLen {
			return strings.TrimSpace(s[:idx]) + "..."
		}
		count++
	}
	return s
}

func bugPersonName(person BugSyncPerson) string {
	if person.Name == nil {
		return ""
	}
	return strings.TrimSpace(*person.Name)
}

func bugPersonHasValue(person BugSyncPerson) bool {
	return person.MateID != nil || bugPersonName(person) != "" || (person.DeptName != nil && strings.TrimSpace(*person.DeptName) != "")
}

func bugAssigneePerson(item BugSyncItem) BugSyncPerson {
	if bugPersonHasValue(item.Assignee) {
		return item.Assignee
	}
	return item.BugDetail.Assignee
}

func trimNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if v := strings.TrimSpace(value); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (i *Importer) warn(msg string, args ...any) {
	if i.Logger != nil {
		i.Logger.Warn(msg, args...)
		return
	}
	slog.Warn(msg, args...)
}

func (req Request) withDefaults(cfg Config) Request {
	if strings.TrimSpace(req.WorkspaceID) == "" {
		req.WorkspaceID = cfg.DefaultWorkspaceID
	}
	if strings.TrimSpace(req.BaseToken) == "" {
		req.BaseToken = strings.TrimSpace(req.AppToken)
	}
	return req
}

func (i *Importer) VerifyToken(header string) error {
	cfg := i.Config.withDefaults()
	if cfg.WebhookToken == "" {
		return ErrNotConfigured
	}
	got := strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(got), "bearer ") {
		got = strings.TrimSpace(got[len("Bearer "):])
	}
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(cfg.WebhookToken)) != 1 {
		return ErrUnauthorized
	}
	return nil
}

func (i *Importer) normalize(req Request) (normalizedRecord, error) {
	wsUUID, err := util.ParseUUID(strings.TrimSpace(req.WorkspaceID))
	if err != nil {
		return normalizedRecord{}, ErrMissingWorkspaceID
	}
	fields, recordID := fieldsFromRequest(req)
	if strings.TrimSpace(req.RecordID) != "" {
		recordID = strings.TrimSpace(req.RecordID)
	}
	if recordID == "" {
		return normalizedRecord{}, ErrMissingRecordID
	}

	m := req.FieldMapping.withDefaults()
	title := textValue(fields[m.Version])
	if title == "" {
		return normalizedRecord{}, ErrMissingTitle
	}
	name := textValue(fields[m.Name])
	notes := textValue(fields[m.Notes])

	rec := normalizedRecord{
		Provider:       firstNonEmpty(strings.TrimSpace(req.Provider), strings.TrimSpace(req.Source), defaultProvider),
		WorkspaceID:    wsUUID,
		BaseToken:      strings.TrimSpace(req.BaseToken),
		TableID:        strings.TrimSpace(req.TableID),
		ViewID:         strings.TrimSpace(req.ViewID),
		RecordID:       recordID,
		RecordURL:      strings.TrimSpace(req.RecordURL),
		VersionType:    textValue(fields[m.VersionType]),
		Title:          title,
		Name:           name,
		Notes:          notes,
		Description:    buildDescription(name, notes, req.RecordURL),
		AssigneeUserID: strings.TrimSpace(req.AssigneeUserID),
		Attachments:    attachmentSources(fields[m.Attachments]),
		RawFields:      fields,
	}
	if req.InstallationID != "" {
		instID, err := util.ParseUUID(strings.TrimSpace(req.InstallationID))
		if err != nil {
			return normalizedRecord{}, fmt.Errorf("invalid installation_id: %w", err)
		}
		rec.InstallationID = instID
	}
	return rec, nil
}

func (m FieldMapping) withDefaults() FieldMapping {
	if strings.TrimSpace(m.VersionType) == "" {
		m.VersionType = "版本类型"
	}
	if strings.TrimSpace(m.Version) == "" {
		m.Version = "版本"
	}
	if strings.TrimSpace(m.Name) == "" {
		m.Name = "需求名称"
	}
	if strings.TrimSpace(m.Notes) == "" {
		m.Notes = "备注"
	}
	if strings.TrimSpace(m.Attachments) == "" {
		m.Attachments = "附件"
	}
	return m
}

func fieldsFromRequest(req Request) (map[string]any, string) {
	_, recordID := recordFieldsAndID(req.Record)
	if len(req.Fields) > 0 {
		return normalizeNumbers(req.Fields), recordID
	}
	fields, recordID := recordFieldsAndID(req.Record)
	return normalizeNumbers(fields), recordID
}

func hasRequestFields(req Request) bool {
	if len(req.Fields) > 0 {
		return true
	}
	fields, _ := recordFieldsAndID(req.Record)
	return len(fields) > 0
}

func (i *Importer) hydrateFields(ctx context.Context, req *Request) error {
	if hasRequestFields(*req) {
		return nil
	}
	req.BaseToken = firstNonEmpty(req.BaseToken, req.AppToken)
	if strings.TrimSpace(req.BaseToken) == "" ||
		strings.TrimSpace(req.TableID) == "" ||
		strings.TrimSpace(req.RecordID) == "" {
		return ErrMissingLarkRecordParams
	}
	fields, err := i.fetchLarkBaseRecord(ctx, *req)
	if err != nil {
		return err
	}
	req.Fields = fields
	return nil
}

func (i *Importer) fetchLarkBaseRecord(ctx context.Context, req Request) (map[string]any, error) {
	token, err := i.larkAppTenantAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	recordPath := "/open-apis/bitable/v1/apps/" + url.PathEscape(req.BaseToken) +
		"/tables/" + url.PathEscape(req.TableID) +
		"/records/" + url.PathEscape(req.RecordID)
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Record struct {
				RecordID string         `json:"record_id"`
				Fields   map[string]any `json:"fields"`
			} `json:"record"`
		} `json:"data"`
	}
	if err := i.doLarkAppJSON(ctx, http.MethodGet, recordPath, token, nil, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("lark base record fetch rejected: code=%d msg=%q", resp.Code, resp.Msg)
	}
	if len(resp.Data.Record.Fields) == 0 {
		return nil, fmt.Errorf("lark base record fetch returned no fields")
	}
	return normalizeNumbers(resp.Data.Record.Fields), nil
}

func (i *Importer) larkAppTenantAccessToken(ctx context.Context) (string, error) {
	cfg := i.Config.withDefaults()
	if cfg.LarkAppID == "" || cfg.LarkAppSecret == "" {
		return "", ErrLarkAppNotConfigured
	}
	now := time.Now()
	i.larkAppTokenMu.Lock()
	if i.larkAppToken.AppID == cfg.LarkAppID &&
		i.larkAppToken.BaseURL == cfg.LarkOpenAPIBaseURL &&
		i.larkAppToken.Value != "" &&
		i.larkAppToken.ExpiresAt.After(now) {
		token := i.larkAppToken.Value
		i.larkAppTokenMu.Unlock()
		return token, nil
	}
	i.larkAppTokenMu.Unlock()

	body := map[string]string{
		"app_id":     cfg.LarkAppID,
		"app_secret": cfg.LarkAppSecret,
	}
	var resp struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int64  `json:"expire"`
	}
	if err := i.doLarkAppJSON(ctx, http.MethodPost, "/open-apis/auth/v3/tenant_access_token/internal", "", body, &resp); err != nil {
		return "", err
	}
	if resp.Code != 0 || resp.TenantAccessToken == "" {
		return "", fmt.Errorf("lark tenant_access_token rejected: code=%d msg=%q", resp.Code, resp.Msg)
	}
	expire := time.Duration(resp.Expire) * time.Second
	if expire < tokenSafetyMargin*2 {
		expire = tokenSafetyMargin * 2
	}
	cached := cachedLarkAppToken{
		AppID:     cfg.LarkAppID,
		BaseURL:   cfg.LarkOpenAPIBaseURL,
		Value:     resp.TenantAccessToken,
		ExpiresAt: time.Now().Add(expire - tokenSafetyMargin),
	}
	i.larkAppTokenMu.Lock()
	i.larkAppToken = cached
	i.larkAppTokenMu.Unlock()
	return resp.TenantAccessToken, nil
}

func (i *Importer) doLarkAppJSON(ctx context.Context, method, endpointPath, bearer string, body any, out any) error {
	cfg := i.Config.withDefaults()
	reqCtx, cancel := context.WithTimeout(ctx, cfg.LarkOpenAPITimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, cfg.LarkOpenAPIBaseURL+endpointPath, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := i.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if isTimeoutError(err) && !errors.Is(ctx.Err(), context.Canceled) {
			return fmt.Errorf("%w: %w", ErrLarkOpenAPITimeout, err)
		}
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("lark openapi returned HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode lark openapi response: %w", err)
	}
	return nil
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func recordFieldsAndID(record json.RawMessage) (map[string]any, string) {
	fields := map[string]any{}
	if len(record) == 0 {
		return fields, ""
	}
	var raw map[string]any
	if err := json.Unmarshal(record, &raw); err != nil {
		return fields, ""
	}
	recordID := firstNonEmpty(
		stringValue(raw["record_id"]),
		stringValue(raw["recordId"]),
		stringValue(raw["id"]),
	)
	if f, ok := raw["fields"].(map[string]any); ok {
		fields = f
	} else {
		fields = make(map[string]any, len(raw))
		for k, v := range raw {
			if k == "record_id" || k == "recordId" || k == "id" {
				continue
			}
			fields[k] = v
		}
	}
	return fields, recordID
}

func buildDescription(name, notes, recordURL string) string {
	var parts []string
	if strings.TrimSpace(name) != "" {
		parts = append(parts, "需求名称："+strings.TrimSpace(name))
	}
	if strings.TrimSpace(notes) != "" {
		parts = append(parts, "备注："+strings.TrimSpace(notes))
	}
	if strings.TrimSpace(recordURL) != "" {
		parts = append(parts, "来源："+strings.TrimSpace(recordURL))
	}
	return strings.Join(parts, "\n\n")
}

func appendAttachmentMarkdown(description string, attachments []db.Attachment) string {
	var blocks []string
	for _, att := range attachments {
		url := strings.TrimSpace(att.Url)
		if url == "" || strings.Contains(description, url) {
			continue
		}
		filename := firstNonEmpty(strings.TrimSpace(att.Filename), defaultDownloadFileName)
		label := escapeMarkdownLabel(filename)
		if isImageAttachment(att) {
			blocks = append(blocks, fmt.Sprintf("![%s](%s)", label, url))
		} else {
			blocks = append(blocks, fmt.Sprintf("!file[%s](%s)", label, url))
		}
	}
	if len(blocks) == 0 {
		return description
	}
	if strings.TrimSpace(description) == "" {
		return strings.Join(blocks, "\n\n")
	}
	return strings.TrimRight(description, "\n") + "\n\n" + strings.Join(blocks, "\n\n")
}

func isImageAttachment(att db.Attachment) bool {
	contentType := strings.ToLower(strings.TrimSpace(att.ContentType))
	if strings.HasPrefix(contentType, "image/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(att.Filename)) {
	case ".apng", ".avif", ".gif", ".jpg", ".jpeg", ".png", ".svg", ".webp":
		return true
	default:
		return false
	}
}

func escapeMarkdownLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '[', ']', '(', ')':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func targetType(v string) string {
	if strings.TrimSpace(v) == "" {
		return defaultTargetType
	}
	return strings.TrimSpace(v)
}

func (i *Importer) resolveMemberByExternalUserID(ctx context.Context, workspaceID pgtype.UUID, externalUserID string) (db.Member, error) {
	externalUserID = strings.TrimSpace(externalUserID)
	if externalUserID == "" {
		return db.Member{}, ErrMissingDefaultAssignee
	}
	member, err := i.lookupMemberByExternalIdentity(ctx, workspaceID, externalUserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Member{}, ErrDefaultAssigneeNotMember
		}
		return db.Member{}, err
	}
	return member, nil
}

func (i *Importer) resolveBugAssigneeMember(ctx context.Context, workspaceID pgtype.UUID, item BugSyncItem, defaultAssignee db.Member) (db.Member, error) {
	person := bugAssigneePerson(item)
	if name := bugPersonName(person); name != "" {
		member, ok, err := i.lookupUniqueMemberByName(ctx, workspaceID, name)
		if err != nil {
			return db.Member{}, err
		}
		if ok {
			return member, nil
		}
	}
	return defaultAssignee, nil
}

func (i *Importer) lookupMemberByExternalIdentity(ctx context.Context, workspaceID pgtype.UUID, externalUserID string) (db.Member, error) {
	externalUserID = strings.TrimSpace(externalUserID)
	if externalUserID == "" {
		return db.Member{}, pgx.ErrNoRows
	}
	return i.Queries.GetWorkspaceMemberByExternalIdentity(ctx, db.GetWorkspaceMemberByExternalIdentityParams{
		WorkspaceID:    workspaceID,
		Provider:       enterpriseLark.ProviderName,
		ExternalUserID: externalUserID,
		OpenID:         externalUserID,
		UnionID:        externalUserID,
	})
}

func (i *Importer) lookupUniqueMemberByName(ctx context.Context, workspaceID pgtype.UUID, name string) (db.Member, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return db.Member{}, false, nil
	}
	members, err := i.Queries.ListMembersWithUser(ctx, workspaceID)
	if err != nil {
		return db.Member{}, false, err
	}
	var matched db.Member
	count := 0
	for _, member := range members {
		if strings.TrimSpace(member.UserName) != name {
			continue
		}
		count++
		if count > 1 {
			return db.Member{}, false, nil
		}
		matched = db.Member{
			ID:          member.ID,
			WorkspaceID: member.WorkspaceID,
			UserID:      member.UserID,
			Role:        member.Role,
			CreatedAt:   member.CreatedAt,
		}
	}
	if count == 0 {
		return db.Member{}, false, nil
	}
	return matched, true, nil
}

func (i *Importer) createAttachments(ctx context.Context, rec normalizedRecord, uploaderID pgtype.UUID) ([]pgtype.UUID, []db.Attachment, []AttachmentError) {
	cfg := i.Config.withDefaults()
	if len(rec.Attachments) == 0 {
		return nil, nil, nil
	}
	if i.Storage == nil {
		return nil, nil, []AttachmentError{{Error: ErrStorageNotConfigured.Error()}}
	}

	sources := rec.Attachments
	if len(sources) > cfg.AttachmentLimit {
		sources = sources[:cfg.AttachmentLimit]
	}
	var ids []pgtype.UUID
	var rows []db.Attachment
	var errs []AttachmentError
	for _, src := range sources {
		body, contentType, filename, err := i.downloadAttachment(ctx, rec, src)
		if err != nil {
			errs = append(errs, AttachmentError{Name: src.Name, Token: src.FileToken, URL: src.URL, Error: err.Error()})
			continue
		}
		if filename == "" {
			filename = firstNonEmpty(src.Name, defaultDownloadFileName)
		}
		id, err := uuid.NewV7()
		if err != nil {
			errs = append(errs, AttachmentError{Name: src.Name, Token: src.FileToken, URL: src.URL, Error: err.Error()})
			continue
		}
		ext := filepath.Ext(filename)
		key := "workspaces/" + util.UUIDToString(rec.WorkspaceID) + "/" + id.String() + ext
		link, err := i.Storage.Upload(ctx, key, body, firstNonEmpty(contentType, src.ContentType, "application/octet-stream"), filename)
		if err != nil {
			errs = append(errs, AttachmentError{Name: src.Name, Token: src.FileToken, URL: src.URL, Error: err.Error()})
			continue
		}
		att, err := i.Queries.CreateAttachment(ctx, db.CreateAttachmentParams{
			ID:           pgtype.UUID{Bytes: id, Valid: true},
			WorkspaceID:  rec.WorkspaceID,
			UploaderType: "member",
			UploaderID:   uploaderID,
			Filename:     filename,
			Url:          link,
			ContentType:  firstNonEmpty(contentType, src.ContentType, "application/octet-stream"),
			SizeBytes:    attachmentSize(src.SizeBytes, len(body)),
		})
		if err != nil {
			errs = append(errs, AttachmentError{Name: src.Name, Token: src.FileToken, URL: src.URL, Error: err.Error()})
			continue
		}
		ids = append(ids, att.ID)
		rows = append(rows, att)
	}
	return ids, rows, errs
}

func attachmentSize(declared int64, actual int) int64 {
	if declared > 0 {
		return declared
	}
	return int64(actual)
}

func (i *Importer) deleteUnlinkedAttachments(ctx context.Context, workspaceID pgtype.UUID, ids []pgtype.UUID) {
	for _, id := range ids {
		if !id.Valid {
			continue
		}
		if err := i.Queries.DeleteAttachment(ctx, db.DeleteAttachmentParams{
			ID:          id,
			WorkspaceID: workspaceID,
		}); err != nil && i.Logger != nil {
			i.Logger.Warn("external issue import: failed to clean up unlinked attachment",
				"attachment_id", util.UUIDToString(id),
				"workspace_id", util.UUIDToString(workspaceID),
				"error", err,
			)
		}
	}
}

func (i *Importer) downloadAttachment(ctx context.Context, rec normalizedRecord, src AttachmentSource) ([]byte, string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, i.Config.withDefaults().AttachmentDownloadTimeout)
	defer cancel()

	if strings.TrimSpace(src.URL) != "" {
		bearer := ""
		if i.looksLikeLarkOpenAPIURL(src.URL) {
			_, token, err := i.larkDownloadAuth(ctx, rec)
			if err != nil {
				return nil, "", "", err
			}
			bearer = token
		}
		body, contentType, filename, err := i.downloadURL(ctx, src.URL, bearer, src.Name)
		if err == nil || strings.TrimSpace(src.TmpURL) == "" || strings.TrimSpace(src.TmpURL) == strings.TrimSpace(src.URL) {
			return body, contentType, filename, err
		}
	}
	if strings.TrimSpace(src.TmpURL) != "" {
		bearer := ""
		if i.looksLikeLarkOpenAPIURL(src.TmpURL) {
			_, token, err := i.larkDownloadAuth(ctx, rec)
			if err != nil {
				return nil, "", "", err
			}
			bearer = token
		}
		return i.downloadURL(ctx, src.TmpURL, bearer, src.Name)
	}
	if strings.TrimSpace(src.FileToken) == "" {
		return nil, "", "", errors.New("attachment has neither url nor file_token")
	}
	return i.downloadLarkFile(ctx, rec, src)
}

func (i *Importer) downloadURL(ctx context.Context, rawURL, bearer, fallbackName string) ([]byte, string, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, "", "", fmt.Errorf("invalid attachment url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := i.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", "", fmt.Errorf("attachment download returned HTTP %d", resp.StatusCode)
	}
	limit := i.Config.withDefaults().AttachmentMaxBytes + 1
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, "", "", err
	}
	if int64(len(body)) >= limit {
		return nil, "", "", fmt.Errorf("attachment exceeds %d bytes", limit-1)
	}
	contentType := resp.Header.Get("Content-Type")
	filename := filenameFromDisposition(resp.Header.Get("Content-Disposition"))
	if filename == "" {
		filename = filenameFromURL(rawURL)
	}
	if filename == "" {
		filename = fallbackName
	}
	if contentType == "" && len(body) > 0 {
		contentType = http.DetectContentType(body[:min(len(body), 512)])
	}
	return body, contentType, filename, nil
}

func (i *Importer) downloadLarkFile(ctx context.Context, rec normalizedRecord, src AttachmentSource) ([]byte, string, string, error) {
	baseURL, token, err := i.larkDownloadAuth(ctx, rec)
	if err != nil {
		return nil, "", "", err
	}
	fileURL := strings.TrimRight(baseURL, "/") + "/open-apis/drive/v1/medias/" + url.PathEscape(src.FileToken) + "/download"
	return i.downloadURL(ctx, fileURL, token, src.Name)
}

func (i *Importer) larkDownloadAuth(ctx context.Context, rec normalizedRecord) (string, string, error) {
	if rec.InstallationID.Valid {
		return i.larkOpenPlatformAuth(ctx, rec)
	}
	token, err := i.larkAppTenantAccessToken(ctx)
	if err != nil {
		return "", "", err
	}
	return i.Config.withDefaults().LarkOpenAPIBaseURL, token, nil
}

type larkOpenPlatformTokenProvider interface {
	TenantAccessToken(context.Context, lark.InstallationCredentials) (string, error)
	OpenPlatformBaseURL(lark.InstallationCredentials) string
}

func (i *Importer) larkOpenPlatformAuth(ctx context.Context, rec normalizedRecord) (string, string, error) {
	if i.LarkInstallations == nil || i.LarkAPIClient == nil || !i.LarkAPIClient.IsConfigured() {
		return "", "", errors.New("lark file download is not configured")
	}
	tokenProvider, ok := i.LarkAPIClient.(larkOpenPlatformTokenProvider)
	if !ok {
		return "", "", errors.New("lark client does not support file downloads")
	}
	inst, err := i.LarkInstallations.GetInWorkspace(ctx, rec.InstallationID, rec.WorkspaceID)
	if err != nil {
		return "", "", err
	}
	secret, err := i.LarkInstallations.DecryptAppSecret(inst)
	if err != nil {
		return "", "", err
	}
	creds := lark.InstallationCredentials{
		AppID:     inst.AppID,
		AppSecret: secret,
		TenantKey: textString(inst.TenantKey),
		Region:    lark.RegionOrDefault(inst.Region),
	}
	token, err := tokenProvider.TenantAccessToken(ctx, creds)
	if err != nil {
		return "", "", err
	}
	return tokenProvider.OpenPlatformBaseURL(creds), token, nil
}

func attachmentSources(v any) []AttachmentSource {
	var out []AttachmentSource
	collectAttachments(v, &out)
	return out
}

func collectAttachments(v any, out *[]AttachmentSource) {
	switch x := v.(type) {
	case nil:
		return
	case []any:
		for _, item := range x {
			collectAttachments(item, out)
		}
	case map[string]any:
		src := AttachmentSource{
			Name:        firstNonEmpty(stringValue(x["name"]), stringValue(x["filename"]), stringValue(x["file_name"])),
			URL:         firstNonEmpty(stringValue(x["url"]), stringValue(x["download_url"]), stringValue(x["file_url"])),
			TmpURL:      stringValue(x["tmp_url"]),
			FileToken:   firstNonEmpty(stringValue(x["file_token"]), stringValue(x["fileToken"]), stringValue(x["token"])),
			ContentType: contentTypeValue(firstNonEmpty(stringValue(x["content_type"]), stringValue(x["mime_type"]), stringValue(x["mime"]), stringValue(x["type"]))),
			SizeBytes:   int64(numberValue(x["size"], x["size_bytes"])),
		}
		if src.Name != "" || src.URL != "" || src.TmpURL != "" || src.FileToken != "" {
			*out = append(*out, src)
			return
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			collectAttachments(x[k], out)
		}
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return
		}
		if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
			*out = append(*out, AttachmentSource{URL: s, Name: filenameFromURL(s)})
		} else {
			*out = append(*out, AttachmentSource{FileToken: s})
		}
	}
}

func textValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strings.TrimSpace(strconv.FormatFloat(x, 'f', -1, 64))
	case bool:
		if x {
			return "true"
		}
		return "false"
	case []any:
		var parts []string
		for _, item := range x {
			if s := textValue(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		for _, key := range []string{"text", "name", "value", "title", "link"} {
			if s := textValue(x[key]); s != "" {
				return s
			}
		}
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func stringValue(v any) string {
	return textValue(v)
}

func contentTypeValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || !strings.Contains(v, "/") {
		return ""
	}
	return v
}

func numberValue(values ...any) float64 {
	for _, v := range values {
		switch x := v.(type) {
		case json.Number:
			if f, err := x.Float64(); err == nil {
				return f
			}
		case float64:
			return x
		case int:
			return float64(x)
		case int64:
			return float64(x)
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				return f
			}
		}
	}
	return 0
}

func sourceOriginID(provider, baseToken, tableID, recordID string) pgtype.UUID {
	h := sha256.Sum256([]byte(strings.Join([]string{
		strings.TrimSpace(provider),
		strings.TrimSpace(baseToken),
		strings.TrimSpace(tableID),
		strings.TrimSpace(recordID),
	}, "\x00")))
	var id uuid.UUID
	copy(id[:], h[:16])
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return pgtype.UUID{Bytes: id, Valid: true}
}

func filenameFromDisposition(disposition string) string {
	if disposition == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(disposition)
	if err != nil {
		return ""
	}
	return filepath.Base(firstNonEmpty(params["filename*"], params["filename"]))
}

func filenameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	name := path.Base(u.Path)
	if name == "." || name == "/" {
		return ""
	}
	return filepath.Base(name)
}

func (i *Importer) looksLikeLarkOpenAPIURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	cfgHost := ""
	if cfgURL, err := url.Parse(i.Config.withDefaults().LarkOpenAPIBaseURL); err == nil {
		cfgHost = strings.ToLower(cfgURL.Host)
	}
	if host != "open.feishu.cn" && host != "open.larksuite.com" && host != cfgHost {
		return false
	}
	return strings.HasPrefix(u.Path, "/open-apis/")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func textString(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && (constraint == "" || pgErr.ConstraintName == constraint)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func DecodeRequest(body io.Reader) (Request, error) {
	var req Request
	dec := json.NewDecoder(body)
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return req, err
	}
	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(raw); err != nil {
		return req, err
	}
	if err := json.NewDecoder(buf).Decode(&req); err != nil {
		return req, err
	}
	if len(req.Fields) == 0 {
		if f, ok := raw["fields"].(map[string]any); ok {
			req.Fields = normalizeNumbers(f)
		}
	}
	if len(req.Record) == 0 {
		if record, ok := raw["record"]; ok {
			if b, err := json.Marshal(record); err == nil {
				req.Record = b
			}
		}
	}
	return req, nil
}

func DecodeBugSyncRequest(body io.Reader) (BugSyncPayload, error) {
	var payload BugSyncPayload
	dec := json.NewDecoder(body)
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return payload, err
	}
	source := any(raw)
	if _, hasItems := raw["items"]; !hasItems {
		if nested, ok := raw["payload"]; ok {
			source = nested
		}
		if data, ok := raw["data"].(map[string]any); ok {
			if nested, ok := data["payload"]; ok {
				source = nested
			}
		}
	}
	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(source); err != nil {
		return payload, err
	}
	if err := json.NewDecoder(buf).Decode(&payload); err != nil {
		return payload, err
	}
	return payload, nil
}

func normalizeNumbers(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = normalizeNumber(v)
	}
	return out
}

func normalizeNumber(v any) any {
	switch x := v.(type) {
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case []any:
		out := make([]any, len(x))
		for idx, item := range x {
			out[idx] = normalizeNumber(item)
		}
		return out
	case map[string]any:
		return normalizeNumbers(x)
	default:
		return v
	}
}
