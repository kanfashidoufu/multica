package externalissue

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type completionRequest struct {
	SourceEnv   string `json:"source_env"`
	BugID       int64  `json:"bug_id"`
	WorkspaceID string `json:"workspace_id"`
	IssueID     string `json:"issue_id"`
	EventID     string `json:"event_id"`
	Revision    int64  `json:"revision"`
	ReopenCount int64  `json:"reopen_count"`
	Outcome     string `json:"outcome"`
}

func completionPayload(j db.LocalSyndraBugCompletion) (completionRequest, bool) {
	bug, err := strconv.ParseInt(j.BugID, 10, 64)
	reopens, err2 := strconv.ParseInt(j.ReopenCount, 10, 64)
	shortKey := "version_bug:" + strconv.FormatInt(bug, 10)
	if err != nil || bug <= 0 || err2 != nil || reopens < 0 || syndraBugEndpoint(j.SourceEnv, "resolve") == "" ||
		(j.RecordKey != shortKey && j.RecordKey != "syndra:"+j.SourceEnv+":"+shortKey) ||
		sourceOriginID("syndra", j.SourceEnv, "version_bug", j.RecordKey) != j.OriginID {
		return completionRequest{}, false
	}
	return completionRequest{SourceEnv: j.SourceEnv, BugID: bug, WorkspaceID: util.UUIDToString(j.WorkspaceID),
		IssueID: util.UUIDToString(j.IssueID), EventID: util.UUIDToString(j.EventID), Revision: j.Revision, ReopenCount: reopens, Outcome: j.Outcome}, true
}

func completionSignature(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("multica:syndra:bug-resolved:v1\x00"))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func postCompletion(ctx context.Context, client *http.Client, secret string, p completionRequest) (string, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, syndraBugEndpoint(p.SourceEnv, "resolve"), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+completionSignature(secret, body))
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Syndra HTTP %d", resp.StatusCode)
	}
	var receipt struct {
		Code    int
		Success bool
		Data    struct {
			EventID string `json:"event_id"`
			Status  string
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&receipt); err != nil {
		return "", err
	}
	if receipt.Success && receipt.Code == 4000 && receipt.Data.EventID == p.EventID {
		switch receipt.Data.Status {
		case "resolved", "noted", "duplicate", "stale", "not_found":
			return receipt.Data.Status, nil
		}
	}
	return "", errors.New("Syndra did not acknowledge this completion")
}

func deliverNextCompletion(ctx context.Context, q *db.Queries, ws pgtype.UUID, client *http.Client, secret string) (bool, error) {
	job, err := q.ClaimSyndraCompletion(ctx, ws)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Metadata is editable. Verify both the immutable importer origin and the
	// current issue/workspace before sending a single byte to another system.
	current, err := q.SyndraCompletionIsDeliverable(ctx, db.SyndraCompletionIsDeliverableParams{IssueID: job.IssueID, WorkspaceID: ws, EventID: job.EventID})
	if err != nil {
		return true, err
	}
	p, ok := completionPayload(job)
	status := "unmapped"
	if ok && current {
		status, err = postCompletion(ctx, client, secret, p)
		if err != nil {
			return true, fmt.Errorf("issue %s event %s attempt %d: %w", p.IssueID, p.EventID, job.Attempts, err)
		}
	}
	if err := q.AckSyndraCompletion(ctx, db.AckSyndraCompletionParams{WorkspaceID: ws, IssueID: job.IssueID, EventID: job.EventID}); err != nil {
		return true, err
	}
	slog.Info("Syndra Bug completion settled", "issue", util.UUIDToString(job.IssueID), "event", util.UUIDToString(job.EventID), "result", status)
	return true, nil
}

// RunSyndraCompletions delivers only committed local transitions. A claim is
// a short lease; network I/O never holds the issue or queue transaction open.
func RunSyndraCompletions(ctx context.Context, q *db.Queries, workspaceID, secret string) {
	ws, err := util.ParseUUID(strings.TrimSpace(workspaceID))
	secret = strings.TrimSpace(secret)
	if err != nil || !ws.Valid || secret == "" {
		return
	}
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		for n := 0; n < 32 && ctx.Err() == nil; n++ {
			found, err := deliverNextCompletion(ctx, q, ws, client, secret)
			if err != nil && ctx.Err() == nil {
				slog.Warn("Syndra completion delivery deferred", "error", err)
			}
			if !found {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
