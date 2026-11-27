package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/externalissue"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/projectaccess"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// No LLM/tool call can supply the human actor: this entry point is wired only
// after the inbound installation, sender binding, membership and invocation checks.
func (h *Handler) SyndraFeishuCommand(ctx context.Context, inst engine.ResolvedInstallation, identity engine.ResolvedIdentity, msg channel.InboundMessage) (string, error) {
	p := strings.Fields(msg.CommandText)
	if len(p) == 0 || (p[0] != "确认分支" && p[0] != "更换分支") || msg.Type != channel.MsgTypeText || msg.Source.ChannelType != channel.TypeFeishu || msg.Source.ChatType != channel.ChatTypeP2P || !inst.Active {
		return "", nil
	}
	// In a quoted reply the adapter keeps the sender's literal text separately.
	// Never reinterpret forwarded text, a card, or enriched history as consent.
	var own lark.InboundMessage
	if json.Unmarshal(msg.Raw, &own) != nil || (own.MessageType != "text" && own.MessageType != "post") || own.CommandBody != msg.CommandText {
		return "", nil
	}
	if own.MessageType == "post" {
		visible, ok := lark.OwnSelectionText(own.MessageType, own.Content)
		if !ok {
			return "", nil
		}
		p = strings.Fields(visible)
		if len(p) == 0 || (p[0] != "确认分支" && p[0] != "更换分支") {
			return "", nil
		}
	}
	ws, err := h.Queries.GetWorkspace(ctx, inst.WorkspaceID)
	if err != nil || ws.Slug != externalissue.ExpertWorkspace {
		return "", err
	}
	agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: ws.ID, ID: inst.AgentID})
	if err != nil {
		return "", err
	}
	ok, err := externalissue.IsWorkspaceBugExpert(ctx, h.Queries, agent, ws.ID)
	if !ok || err != nil {
		return "", err
	}
	if (len(p) != 3 && len(p) != 4) || !strings.HasPrefix(p[1], ws.IssuePrefix+"-") || !externalissue.ValidBranch(p[len(p)-1]) {
		return "", nil // Not the precise shortcut: let the expert interpret the conversation.
	}
	number, err := strconv.ParseInt(strings.TrimPrefix(p[1], ws.IssuePrefix+"-"), 10, 32)
	if err != nil || number < 1 || msg.MessageID == "" {
		return "", nil
	}
	reply, err := h.recordFeishuBranch(ctx, inst, identity.UserID, msg.MessageID, ws, int32(number), p[0], strings.Join(p[2:], " "), nil)
	if errors.Is(err, externalissue.ErrBranchInput) {
		return "", nil
	} // Let the expert help with ambiguity, including wrong or missing repo context.
	return reply, err
}

func (h *Handler) recordFeishuBranch(ctx context.Context, inst engine.ResolvedInstallation, user pgtype.UUID, messageID string, ws db.Workspace, number int32, verb, choice string, evidence *externalissue.FeishuBranchEvidence) (string, error) {
	const denied = "未找到你可确认的 Bug，请使用原 Bug 编号，并核对当前 Syndra 负责人。"
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	if evidence != nil {
		if err := validateBranchTurnWrite(ctx, q, inst, user, evidence.SourceTaskID, messageID); err != nil {
			return "", err
		}
	}
	bug, err := q.GetIssueByNumber(ctx, db.GetIssueByNumberParams{WorkspaceID: ws.ID, Number: number})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", externalissue.ErrBranchInput, denied)
	}
	if err != nil {
		return "", err
	}
	if !externalissue.IsSyndraBug(bug) || bug.AssigneeID != inst.AgentID || bug.AssigneeType.String != "agent" {
		return "", fmt.Errorf("%w: %s", externalissue.ErrBranchInput, denied)
	}
	member, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{WorkspaceID: ws.ID, UserID: user})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", externalissue.ErrBranchInput, denied)
	}
	if err != nil {
		return "", err
	}
	vis, err := projectaccess.ForMember(ctx, q, member)
	if err != nil {
		return "", err
	}
	if !vis.CanSeeProject(bug.ProjectID) {
		return "", fmt.Errorf("%w: %s", externalissue.ErrBranchInput, denied)
	}
	// Serialize the receipt lookup with other deliveries of this message and
	// recheck the assignment after any concurrent Syndra sync.
	bug, err = q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{WorkspaceID: ws.ID, ID: bug.ID})
	if err != nil {
		return "", err
	}
	if bug.AssigneeType.String != "agent" || bug.AssigneeID != inst.AgentID || !vis.CanSeeProject(bug.ProjectID) || !externalissue.IsSyndraBug(bug) {
		return "", fmt.Errorf("%w: %s", externalissue.ErrBranchInput, denied)
	}
	text := "默认 " + choice
	if verb == "更换分支" {
		text = "更换默认 " + choice
	}
	id := pgtype.UUID{Bytes: uuid.NewSHA1(inst.ID.Bytes, []byte("syndra-branch:"+messageID)), Valid: true}
	branch := strings.Fields(choice)
	ack := fmt.Sprintf("已在 %s 记下你选的分支 %s。修复任务会自动续跑，不用再去 Multica 操作。", service.IssueIdentifier(ws.IssuePrefix, number), branch[len(branch)-1])
	prior, err := q.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: ws.ID, ID: id})
	if err == nil {
		if prior.IssueID != bug.ID || prior.AuthorType != "member" || prior.AuthorID != user || prior.Content != text || prior.DeletedAt.Valid {
			return "", fmt.Errorf("%w: %s", externalissue.ErrBranchInput, denied)
		}
		return ack, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	var sourceTask pgtype.UUID
	if evidence != nil {
		sourceTask = evidence.SourceTaskID
	}
	row, err := q.CreateComment(ctx, db.CreateCommentParams{ID: id, WorkspaceID: ws.ID, IssueID: bug.ID, AuthorType: "member", AuthorID: user, Content: text, Type: "comment", SourceTaskID: sourceTask})
	if err != nil {
		return "", err
	}
	i := h.syndraBranchImporter(q)
	ticket, changed, err := i.ApplyFeishuBranchReply(ctx, bug, row.Comment(), evidence)
	if err != nil {
		return "", err
	}
	if !ticket.ID.Valid {
		return "", fmt.Errorf("%w: %s", externalissue.ErrBranchInput, denied)
	}
	var cancelled []db.AgentTaskQueue
	if changed {
		cancelled, err = service.StopClosedIssueWakeups(ctx, q, ticket)
		if err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	actor := uuidToString(user)
	resp := commentToResponse(row.Comment(), nil, nil)
	resp.IssueRevision = row.IssueRevision
	h.publish(protocol.EventCommentCreated, uuidToString(ws.ID), "member", actor, map[string]any{"comment": resp, "issue_title": bug.Title, "issue_status": bug.Status, "issue_revision": row.IssueRevision})
	if changed {
		h.publishIssueSnapshots(ctx, []db.Issue{ticket}, "member", actor)
	}
	if len(cancelled) > 0 {
		h.TaskService.BroadcastCancelledTasks(ctx, uuidToString(ws.ID), cancelled)
	}
	return ack, nil
}
