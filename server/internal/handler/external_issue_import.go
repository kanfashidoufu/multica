package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/externalissue"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (h *Handler) ImportExternalIssue(w http.ResponseWriter, r *http.Request) {
	buildIssueResponse := func(ctx context.Context, issue db.Issue, attachments []db.Attachment) IssueResponse {
		prefix := h.getIssuePrefix(ctx, issue.WorkspaceID)
		resp := issueToResponse(issue, prefix)
		if len(attachments) > 0 {
			resp.Attachments = make([]AttachmentResponse, len(attachments))
			for idx, att := range attachments {
				resp.Attachments[idx] = h.attachmentToResponse(ctx, att, attachmentURLModeSigned)
			}
		}
		return resp
	}
	importer := &externalissue.Importer{
		Queries:      h.Queries,
		IssueService: h.IssueService,
		Bus:          h.Bus,
		BroadcastPayload: func(ctx context.Context, issue db.Issue, attachments []db.Attachment) map[string]any {
			return map[string]any{"issue": buildIssueResponse(ctx, issue, attachments)}
		},
		Logger: slog.Default(),
		Config: externalissue.Config{
			WebhookToken:           os.Getenv("MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN"),
			BugWorkspaceID:         os.Getenv("MULTICA_EXTERNAL_BUG_WORKSPACE_ID"),
			RequirementWorkspaceID: os.Getenv("MULTICA_EXTERNAL_REQUIREMENT_WORKSPACE_ID"),
		},
	}
	if err := importer.VerifyToken(r.Header.Get("Authorization")); err != nil {
		writeExternalImportError(w, err)
		return
	}
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sync_type"))) {
	case "bug":
		handleExternalBugSync(w, r, importer, buildIssueResponse)
	case "requirement":
		handleExternalRequirementSync(w, r, importer, buildIssueResponse)
	default:
		writeError(w, http.StatusBadRequest, "unsupported external issue sync_type")
	}
}

func handleExternalBugSync(w http.ResponseWriter, r *http.Request, importer *externalissue.Importer, buildIssueResponse func(context.Context, db.Issue, []db.Attachment) IssueResponse) {
	if err := logExternalBugSyncRequestBody(r); err != nil {
		slog.Warn("external bug sync request body read failed",
			append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusBadRequest, "invalid bug sync request body")
		return
	}
	req, err := decodeExternalBugSyncRequest(r)
	if err != nil {
		slog.Warn("external bug sync decode failed",
			append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusBadRequest, "invalid bug sync request body")
		return
	}
	res, err := importer.ImportBugSync(r.Context(), req)
	if err != nil {
		slog.Warn("external bug sync failed", append(logger.RequestAttrs(r), "error", err)...)
		writeExternalImportError(w, err)
		return
	}
	items := make([]map[string]any, 0, len(res.Items))
	created := false
	synced := 0
	ignored := 0
	for _, item := range res.Items {
		body := map[string]any{
			"provider":         item.Provider,
			"source_record_id": item.SourceRecordID,
			"external_key":     item.ExternalKey,
			"bug_id":           item.BugID,
		}
		if item.Ignored {
			ignored++
			body["status"] = "ignored"
			body["reason"] = item.Reason
		} else {
			synced++
			body["status"] = "synced"
			body["existing"] = item.Existing
			if !item.Existing {
				created = true
			}
			if item.Issue.ID.Valid {
				body["issue"] = buildIssueResponse(r.Context(), item.Issue, nil)
			}
		}
		items = append(items, body)
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	resp := map[string]any{
		"status":     "synced",
		"sync_type":  "bug",
		"provider":   res.Provider,
		"item_count": len(res.Items),
		"synced":     synced,
		"ignored":    ignored,
		"items":      items,
	}
	if len(res.Items) == 1 && !res.Items[0].Ignored {
		item := res.Items[0]
		resp["existing"] = item.Existing
		resp["source_record_id"] = item.SourceRecordID
		resp["external_key"] = item.ExternalKey
		resp["bug_id"] = item.BugID
		resp["issue"] = buildIssueResponse(r.Context(), item.Issue, nil)
	}
	writeJSON(w, status, resp)
}

func logExternalBugSyncRequestBody(r *http.Request) error {
	if r.Body == nil {
		slog.Info("external bug sync request body received",
			append(logger.RequestAttrs(r), "request_body_bytes", 0, "request_body", "")...)
		return nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	slog.Info("external bug sync request body received",
		append(logger.RequestAttrs(r), "request_body_bytes", len(body), "request_body", string(body))...)
	return nil
}

func handleExternalRequirementSync(w http.ResponseWriter, r *http.Request, importer *externalissue.Importer, buildIssueResponse func(context.Context, db.Issue, []db.Attachment) IssueResponse) {
	_, err := logExternalRequirementSyncRequestBody(r)
	if err != nil {
		slog.Warn("external requirement sync request body read failed",
			append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusBadRequest, "invalid requirement sync request body")
		return
	}
	payload, err := externalissue.DecodeRequirementSyncRequest(r.Body)
	if err != nil {
		slog.Warn("external requirement sync decode failed",
			append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusBadRequest, "invalid requirement sync request body")
		return
	}
	res, err := importer.ImportRequirementSync(r.Context(), payload)
	if err != nil {
		slog.Warn("external requirement sync failed", append(logger.RequestAttrs(r), "error", err)...)
		writeExternalImportError(w, err)
		return
	}

	status := http.StatusOK
	if !res.Existing {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"status":               "synced",
		"sync_type":            "requirement",
		"provider":             res.Provider,
		"existing":             res.Existing,
		"source_record_id":     res.SourceRecordID,
		"product_iteration_id": res.ProductIterationID,
		"project_record_url":   res.ProjectRecordURL,
		"issue":                buildIssueResponse(r.Context(), res.Issue, nil),
	})
}

func logExternalRequirementSyncRequestBody(r *http.Request) (int, error) {
	if r.Body == nil {
		slog.Info("external syndra requirement sync request body received",
			append(logger.RequestAttrs(r), "request_body_bytes", 0, "request_body", "")...)
		return 0, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return 0, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	slog.Info("external syndra requirement sync request body received",
		append(logger.RequestAttrs(r), "request_body_bytes", len(body), "request_body", string(body))...)
	return len(body), nil
}

func decodeExternalBugSyncRequest(r *http.Request) (externalissue.BugSyncRequest, error) {
	var req externalissue.BugSyncRequest
	if r.Body == nil {
		return req, io.EOF
	}
	payload, err := externalissue.DecodeBugSyncRequest(r.Body)
	if err != nil {
		return req, err
	}
	req.Payload = payload
	return req, nil
}

func writeExternalImportError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, externalissue.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "invalid external issue webhook token")
	case errors.Is(err, externalissue.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "external issue sync is not configured")
	case errors.Is(err, externalissue.ErrRequirementWorkspaceNotConfigured):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, externalissue.ErrBugWorkspaceNotConfigured):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, externalissue.ErrBugWorkspaceOwnerNotFound):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, externalissue.ErrMissingWorkspaceID),
		errors.Is(err, externalissue.ErrMissingRecordID),
		errors.Is(err, externalissue.ErrMissingTitle),
		errors.Is(err, externalissue.ErrMissingExecutionPrompt),
		errors.Is(err, externalissue.ErrMissingExecutorID),
		errors.Is(err, externalissue.ErrInvalidExecutorID),
		errors.Is(err, externalissue.ErrMissingRequirementOwnerID),
		errors.Is(err, externalissue.ErrMissingProductIterationID),
		errors.Is(err, externalissue.ErrMissingProjectRecordURL):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, externalissue.ErrMissingDefaultAssignee),
		errors.Is(err, externalissue.ErrDefaultAssigneeNotMember),
		errors.Is(err, externalissue.ErrRequirementExecutorNotFound),
		errors.Is(err, externalissue.ErrRequirementOwnerNotMember),
		errors.Is(err, externalissue.ErrRequirementAgentNotReady):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, externalissue.ErrRequirementExecutorConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "failed to sync external issue")
	}
}
