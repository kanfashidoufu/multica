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
	var own struct{ MessageType, CommandBody string }
	if json.Unmarshal(msg.Raw, &own) != nil || (own.MessageType != "text" && own.MessageType != "post") || own.CommandBody != msg.CommandText {
		return "", nil
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
	const usage = "用法：确认分支 原Bug编号 [仓库] 分支名。例如：确认分支 QTB-1974 feature/v3.1609_wn。"
	if (len(p) != 3 && len(p) != 4) || !strings.HasPrefix(p[1], ws.IssuePrefix+"-") || !externalissue.ValidBranch(p[len(p)-1]) {
		return usage, nil
	}
	number, err := strconv.ParseInt(strings.TrimPrefix(p[1], ws.IssuePrefix+"-"), 10, 32)
	if err != nil || number < 1 || msg.MessageID == "" {
		return usage, nil
	}
	const denied = "未找到你可确认的 Bug，请使用原 Bug 编号，并核对当前 Syndra 负责人。"
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	bug, err := q.GetIssueByNumber(ctx, db.GetIssueByNumberParams{WorkspaceID: ws.ID, Number: int32(number)})
	if errors.Is(err, pgx.ErrNoRows) {
		return denied, nil
	}
	if err != nil {
		return "", err
	}
	if !externalissue.IsSyndraBug(bug) || bug.AssigneeID != inst.AgentID || bug.AssigneeType.String != "agent" {
		return denied, nil
	}
	member, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{WorkspaceID: ws.ID, UserID: identity.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return denied, nil
	}
	if err != nil {
		return "", err
	}
	vis, err := projectaccess.ForMember(ctx, q, member)
	if err != nil {
		return "", err
	}
	if !vis.CanSeeProject(bug.ProjectID) {
		return denied, nil
	}
	text := "默认 " + strings.Join(p[2:], " ")
	if p[0] == "更换分支" {
		text = "更换默认 " + strings.Join(p[2:], " ")
	}
	id := pgtype.UUID{Bytes: uuid.NewSHA1(inst.ID.Bytes, []byte("syndra-branch:"+msg.MessageID)), Valid: true}
	ack := fmt.Sprintf("已在 %s 记录你的分支确认：%s。无需手动完成确认单或恢复 Bug。", p[1], p[len(p)-1])
	prior, err := q.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{WorkspaceID: ws.ID, ID: id})
	if err == nil {
		if prior.IssueID != bug.ID || prior.AuthorID != identity.UserID || prior.Content != text || prior.DeletedAt.Valid {
			return denied, nil
		}
		return ack, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	row, err := q.CreateComment(ctx, db.CreateCommentParams{ID: id, WorkspaceID: ws.ID, IssueID: bug.ID, AuthorType: "member", AuthorID: identity.UserID, Content: text, Type: "comment"})
	if err != nil {
		return "", err
	}
	i := h.syndraBranchImporter(q)
	ticket, changed, err := i.ApplyInlineBranchReply(ctx, bug, row.Comment())
	if errors.Is(err, externalissue.ErrBranchInput) {
		return strings.TrimPrefix(err.Error(), externalissue.ErrBranchInput.Error()+": "), nil
	}
	if err != nil {
		return "", err
	}
	if !ticket.ID.Valid {
		return denied, nil
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
	actor := uuidToString(identity.UserID)
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
