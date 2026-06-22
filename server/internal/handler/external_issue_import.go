package handler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	enterpriseLark "github.com/multica-ai/multica/server/internal/enterprise/lark"
	"github.com/multica-ai/multica/server/internal/externalissue"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (h *Handler) ImportExternalIssue(w http.ResponseWriter, r *http.Request) {
	larkCfg := enterpriseLark.ConfigFromEnv()
	buildIssueResponse := func(ctx context.Context, issue db.Issue, attachments []db.Attachment) IssueResponse {
		prefix := h.getIssuePrefix(ctx, issue.WorkspaceID)
		resp := issueToResponse(issue, prefix)
		if len(attachments) > 0 {
			resp.Attachments = make([]AttachmentResponse, len(attachments))
			for idx, att := range attachments {
				resp.Attachments[idx] = h.attachmentToResponse(ctx, att)
			}
		}
		return resp
	}
	importer := &externalissue.Importer{
		Queries:           h.Queries,
		IssueService:      h.IssueService,
		Storage:           h.Storage,
		LarkInstallations: h.LarkInstallations,
		LarkAPIClient:     h.LarkAPIClient,
		Bus:               h.Bus,
		BroadcastPayload: func(ctx context.Context, issue db.Issue, attachments []db.Attachment) map[string]any {
			return map[string]any{"issue": buildIssueResponse(ctx, issue, attachments)}
		},
		Logger: slog.Default(),
		Config: externalissue.Config{
			WebhookToken:                  os.Getenv("MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN"),
			DefaultWorkspaceID:            os.Getenv("MULTICA_EXTERNAL_ISSUE_DEFAULT_WORKSPACE_ID"),
			DefaultAssigneeExternalUserID: os.Getenv("MULTICA_EXTERNAL_ISSUE_DEFAULT_LARK_USER_ID"),
			LarkAppID:                     larkCfg.AppID,
			LarkAppSecret:                 larkCfg.AppSecret,
			LarkOpenAPIBaseURL:            strings.TrimSpace(os.Getenv("MULTICA_LARK_HTTP_BASE_URL")),
			LarkOpenAPITimeout:            externalIssueDurationEnv("MULTICA_EXTERNAL_ISSUE_LARK_OPENAPI_TIMEOUT"),
		},
	}
	if err := importer.VerifyToken(r.Header.Get("Authorization")); err != nil {
		writeExternalImportError(w, err)
		return
	}

	req, err := decodeExternalIssueImportRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	res, err := importer.Import(r.Context(), req)
	if err != nil {
		slog.Warn("external issue import failed", append(logger.RequestAttrs(r), "error", err)...)
		writeExternalImportError(w, err)
		return
	}
	if res.Ignored {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":           "ignored",
			"reason":           res.Reason,
			"provider":         res.Provider,
			"source_record_id": res.SourceRecordID,
		})
		return
	}

	resp := buildIssueResponse(r.Context(), res.Issue, res.Attachments)
	status := http.StatusCreated
	if res.Existing {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"status":            "synced",
		"existing":          res.Existing,
		"provider":          res.Provider,
		"source_record_id":  res.SourceRecordID,
		"issue":             resp,
		"attachment_errors": res.AttachmentErrors,
	})
}

func decodeExternalIssueImportRequest(r *http.Request) (externalissue.Request, error) {
	var req externalissue.Request
	if r.Body != nil && r.ContentLength != 0 {
		var err error
		req, err = externalissue.DecodeRequest(r.Body)
		if err != nil && !errors.Is(err, io.EOF) {
			return req, err
		}
	}
	applyExternalIssueQueryParams(&req, r.URL.Query())
	return req, nil
}

func applyExternalIssueQueryParams(req *externalissue.Request, values url.Values) {
	setStringParam(values, "provider", &req.Provider)
	setStringParam(values, "source", &req.Source)
	setStringParam(values, "workspace_id", &req.WorkspaceID)
	setStringParam(values, "installation_id", &req.InstallationID)
	setStringParam(values, "app_token", &req.AppToken)
	setStringParam(values, "base_token", &req.BaseToken)
	setStringParam(values, "table_id", &req.TableID)
	setStringParam(values, "view_id", &req.ViewID)
	setStringParam(values, "record_id", &req.RecordID)
	setStringParam(values, "record_url", &req.RecordURL)
	setStringParam(values, "target_type", &req.TargetType)
	setStringParam(values, "assignee_user_id", &req.AssigneeUserID)
	if strings.EqualFold(strings.TrimSpace(values.Get("allow_duplicate")), "true") {
		req.AllowDuplicate = true
	}
}

func setStringParam(values url.Values, key string, target *string) {
	if v := strings.TrimSpace(values.Get(key)); v != "" {
		*target = v
	}
}

func writeExternalImportError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, externalissue.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "invalid external issue webhook token")
	case errors.Is(err, externalissue.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "external issue import is not configured")
	case errors.Is(err, externalissue.ErrMissingWorkspaceID),
		errors.Is(err, externalissue.ErrMissingRecordID),
		errors.Is(err, externalissue.ErrMissingTitle),
		errors.Is(err, externalissue.ErrMissingLarkRecordParams):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, externalissue.ErrLarkAppNotConfigured):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, externalissue.ErrLarkOpenAPITimeout):
		writeError(w, http.StatusGatewayTimeout, err.Error())
	case errors.Is(err, externalissue.ErrMissingDefaultAssignee),
		errors.Is(err, externalissue.ErrDefaultAssigneeNotMember):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "failed to import external issue")
	}
}

func externalIssueDurationEnv(key string) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}
