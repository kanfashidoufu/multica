package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/externalissue"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type branchChatTurn struct {
	ws       db.Workspace
	user     pgtype.UUID
	inst     lark.Installation
	delivery db.ChannelTaskDelivery
}

var errBranchChat = errors.New("这项操作只适用于负责人本人和修复专家的当前飞书私聊")

// Fence a slow interpretation (including the source-message HTTP read) against
// rebinding, /clear, or a newer message before it writes a human receipt.
func validateBranchTurnWrite(ctx context.Context, q *db.Queries, inst engine.ResolvedInstallation, actor, taskID pgtype.UUID, message string) error {
	task, err := q.GetAgentTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.Status != "running" || task.OriginatorUserID != actor || task.AgentID != inst.AgentID || !task.ChatSessionID.Valid {
		return errBranchChat
	}
	d, err := q.GetChannelTaskDelivery(ctx, taskID)
	if err != nil {
		return err
	}
	b, err := q.LockChannelChatSessionBindingForContext(ctx, task.ChatSessionID)
	if err != nil {
		return err
	}
	if b.RetiredAt.Valid || b.ID != d.BindingID || b.InstallationID != inst.ID || d.InstallationID != inst.ID || d.ChatType != "p2p" || d.ChannelType != "feishu" || b.LastMessageID.String != message || d.ChannelMessageID.String != message || d.RouteRevision != b.RouteRevision || task.ChannelContextRevision.Int64 != b.ContextRevision {
		return errBranchChat
	}
	app, err := q.GetChannelInstallationInWorkspace(ctx, db.GetChannelInstallationInWorkspaceParams{WorkspaceID: inst.WorkspaceID, ID: inst.ID, ChannelType: "feishu"})
	if err != nil {
		return err
	}
	if app.AgentID != inst.AgentID || app.Status != "active" {
		return errBranchChat
	}
	u, err := q.GetChannelUserBindingByUserID(ctx, db.GetChannelUserBindingByUserIDParams{InstallationID: inst.ID, ChannelUserID: d.ChannelSenderID.String})
	if err != nil {
		return err
	}
	if u.WorkspaceID != inst.WorkspaceID || u.MulticaUserID != actor {
		return errBranchChat
	}
	return nil
}

// Neither an agent-owned web chat nor a previous DM establishes authority for
// this turn. Use the task token and its immutable Feishu delivery, then verify
// the current installation and binding before accepting an interpreted choice.
func (h *Handler) branchChatTurn(r *http.Request) (t branchChatTurn, err error) {
	ctx := r.Context()
	if r.Header.Get("X-Actor-Source") != "task_token" || h.LarkInstallations == nil || h.LarkAPIClient == nil {
		return t, errBranchChat
	}
	task, ok := h.taskFromRequestHeader(r)
	if !ok || task.Status != "running" || !task.ChatSessionID.Valid || !task.ChatInputTaskID.Valid || !task.OriginatorUserID.Valid || uuidToString(task.AgentID) != r.Header.Get("X-Agent-ID") {
		return t, errBranchChat
	}
	ws, err := h.Queries.GetWorkspaceBySlug(ctx, externalissue.ExpertWorkspace)
	if err != nil {
		return t, err
	}
	if uuidToString(ws.ID) != r.Header.Get("X-Workspace-ID") {
		return t, errBranchChat
	}
	ingested, err := h.Queries.TaskHasChannelIngestedMessages(ctx, task.ChatInputTaskID)
	if err != nil {
		return t, err
	}
	if !ingested {
		return t, errBranchChat
	}
	d, err := h.Queries.GetChannelTaskDelivery(ctx, task.ID)
	if err != nil {
		return t, err
	}
	if d.ChannelType != string(channel.TypeFeishu) || d.ChatType != string(channel.ChatTypeP2P) || d.ChannelMessageID.String == "" || d.ChannelSenderID.String == "" {
		return t, errBranchChat
	}
	b, err := h.Queries.GetChannelChatSessionBindingBySession(ctx, db.GetChannelChatSessionBindingBySessionParams{ChatSessionID: task.ChatSessionID, ChannelType: d.ChannelType})
	if err != nil {
		return t, err
	}
	if b.RetiredAt.Valid || b.ID != d.BindingID || b.InstallationID != d.InstallationID || b.RouteRevision != d.RouteRevision || !task.ChannelContextRevision.Valid || task.ChannelContextRevision.Int64 != b.ContextRevision || b.LastMessageID.String != d.ChannelMessageID.String {
		return t, errBranchChat
	}
	inst, err := h.LarkInstallations.GetInWorkspace(ctx, d.InstallationID, ws.ID)
	if err != nil {
		return t, err
	}
	if inst.Status != string(lark.InstallationActive) || inst.AgentID != task.AgentID {
		return t, errBranchChat
	}
	a, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: ws.ID, ID: inst.AgentID})
	if err != nil {
		return t, err
	}
	expert, err := externalissue.IsWorkspaceBugExpert(ctx, h.Queries, a, ws.ID)
	if err != nil {
		return t, err
	}
	if !expert {
		return t, errBranchChat
	}
	u, err := h.Queries.GetChannelUserBindingByUserID(ctx, db.GetChannelUserBindingByUserIDParams{InstallationID: inst.ID, ChannelUserID: d.ChannelSenderID.String})
	if err != nil {
		return t, err
	}
	if u.WorkspaceID != ws.ID || u.MulticaUserID != task.OriginatorUserID || u.ChannelType != d.ChannelType {
		return t, errBranchChat
	}
	if _, err = h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{WorkspaceID: ws.ID, UserID: u.MulticaUserID}); err != nil {
		return t, err
	}
	return branchChatTurn{ws, u.MulticaUserID, inst, d}, nil
}

type branchChatOption struct {
	Issue string `json:"issue"`
	Title string `json:"title"`
	externalissue.BranchOption
}

func branchChatScopeMatches(options []branchChatOption, truncated bool, prefix, repo, branch string, e externalissue.FeishuBranchEvidence) bool {
	if !e.AllowsIssue(prefix, true) {
		return false
	}
	namedIssue := e.AllowsIssue(prefix, false)
	if truncated && !namedIssue {
		return false
	}
	// Use words from the human or the preceding anchored offer, never a repo
	// inferred by the model alone, to break a tie between two waiting bugs.
	text := e.UserText
	if e.PriorOffer != "" && !strings.Contains(text, branch) && !strings.Contains(branch, e.Fragment) {
		text += " " + e.PriorOffer
	}
	mentions := func(name string) bool {
		return regexp.MustCompile(`(^|[^A-Za-z0-9._/-])` + regexp.QuoteMeta(name) + `($|[^A-Za-z0-9._/-])`).MatchString(text)
	}
	namedFull, namedShort := false, false
	for _, o := range options {
		if namedIssue && o.Issue != e.IssueKey {
			continue
		}
		namedFull = namedFull || mentions(o.Repo)
		namedShort = namedShort || mentions(path.Base(o.Repo))
	}
	n := 0
	target := false
	for _, o := range options {
		if namedIssue && o.Issue != e.IssueKey || namedFull && !mentions(o.Repo) || !namedFull && namedShort && !mentions(path.Base(o.Repo)) {
			continue
		}
		possible := len(o.Candidates) == 0 || slices.Contains(o.Candidates, branch)
		if !possible && e.Fragment != "" && strings.Contains(branch, e.Fragment) {
			possible = slices.ContainsFunc(o.Candidates, func(ref string) bool { return strings.Contains(ref, e.Fragment) })
		}
		if possible {
			n++
			target = target || o.Issue == e.IssueKey && o.Repo == repo
		}
	}
	return n == 1 && target
}

func (h *Handler) branchChatOptions(ctx context.Context, t branchChatTurn, specific string) ([]branchChatOption, bool, error) {
	// Bound discovery to the member's own native waits; include consumed
	// one-shots because a manually closed legacy anchor may still lack a receipt.
	var ids []pgtype.UUID
	if specific != "" {
		n, err := strconv.ParseInt(strings.TrimPrefix(specific, t.ws.IssuePrefix+"-"), 10, 32)
		if err != nil || n < 1 || service.IssueIdentifier(t.ws.IssuePrefix, int32(n)) != specific {
			return nil, false, externalissue.ErrBranchInput
		}
		bug, err := h.Queries.GetIssueByNumber(ctx, db.GetIssueByNumberParams{WorkspaceID: t.ws.ID, Number: int32(n)})
		if errors.Is(err, pgx.ErrNoRows) {
			return []branchChatOption{}, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		if bug.AssigneeType.String == "agent" && bug.AssigneeID == t.inst.AgentID {
			ids = []pgtype.UUID{bug.ID}
		}
	} else {
		rows, err := h.DB.Query(ctx, `SELECT i.id FROM issue i JOIN issue_wakeup w ON w.issue_id=i.id AND w.workspace_id=i.workspace_id
		WHERE i.workspace_id=$1 AND i.assignee_type='agent' AND i.assignee_id=$2 AND w.agent_id=$2 AND w.created_by=$3
		AND i.status NOT IN ('done','cancelled') AND w.disabled_at IS NULL AND w.paused_reason IS NULL AND w.timed_out_at IS NULL
		AND (w.enabled OR w.last_task_id IS NOT NULL) AND starts_with(w.instruction,$4)
		GROUP BY i.id ORDER BY max(w.created_at) DESC,i.id LIMIT 51`, t.ws.ID, t.inst.AgentID, t.user, externalissue.BranchWaitInstruction)
		if err != nil {
			return nil, false, err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
		if err != nil {
			return nil, false, err
		}
	}
	truncated := len(ids) > 50
	i := h.syndraBranchImporter(h.Queries)
	out := []branchChatOption{}
	for _, id := range ids[:min(50, len(ids))] {
		bug, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: t.ws.ID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		opts, err := i.PendingBranchOptions(ctx, bug, t.user)
		if errors.Is(err, externalissue.ErrBranchInput) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		for _, o := range opts {
			out = append(out, branchChatOption{service.IssueIdentifier(t.ws.IssuePrefix, bug.Number), bug.Title, o})
		}
	}
	return out, truncated, nil
}

// priorBranchOffer only reads an immediately preceding, recent message by
// this bot in the same DM. An old offer, another sender or a forwarded card
// must not turn a context-free "yes" into a branch decision.
func (h *Handler) priorBranchOffer(ctx context.Context, t branchChatTurn, creds lark.InstallationCredentials, current lark.LarkMessage) string {
	ms, err := strconv.ParseInt(current.CreateTime, 10, 64)
	if err != nil || ms <= 0 {
		return ""
	}
	items, err := h.LarkAPIClient.ListChatMessages(ctx, creds, lark.ListMessagesParams{ChatID: lark.ChatID(t.delivery.ChannelChatID), PageSize: 8, EndTime: ms/1000 + 1})
	if err != nil {
		return ""
	}
	idx := slices.IndexFunc(items, func(m lark.LarkMessage) bool { return m.MessageID == current.MessageID })
	if idx < 0 || idx+1 >= len(items) {
		return ""
	}
	prev := items[idx+1]
	at, err := strconv.ParseInt(prev.CreateTime, 10, 64)
	if err != nil || at > ms || ms-at > int64(30*time.Minute/time.Millisecond) || prev.Deleted || prev.UpperMessageID != "" || prev.SenderType != "app" || (prev.SenderID != t.inst.AppID && prev.SenderID != t.inst.BotOpenID) {
		return ""
	}
	text, _ := lark.OwnSelectionText(prev.MessageType, prev.Content)
	return text
}

func (h *Handler) SyndraFeishuConversation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := h.branchChatTurn(r)
	if err != nil {
		if errors.Is(err, errBranchChat) || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, lark.ErrInstallationNotFound) {
			writeError(w, 403, errBranchChat.Error())
		} else {
			writeError(w, 500, "无法核对飞书私聊身份")
		}
		return
	}
	filter := ""
	if r.Method == http.MethodGet {
		filter = r.URL.Query().Get("issue")
	}
	options, more, err := h.branchChatOptions(ctx, t, filter)
	if err != nil {
		branchError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"options": options, "truncated": more})
		return
	}
	var in struct {
		Issue, Repo, Branch, Fragment string
		Replace                       bool
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil || in.Fragment == "" || !externalissue.ValidBranch(in.Branch) {
		writeError(w, 400, "需要明确的分支和本人这条消息中的选择依据")
		return
	}
	num, err := strconv.ParseInt(strings.TrimPrefix(in.Issue, t.ws.IssuePrefix+"-"), 10, 32)
	if err != nil || num < 1 || service.IssueIdentifier(t.ws.IssuePrefix, int32(num)) != in.Issue {
		writeError(w, 400, "无法确定原 Bug")
		return
	}
	creds, err := t.inst.Credentials(h.LarkInstallations)
	if err != nil {
		writeError(w, 503, "暂时无法核实飞书原消息")
		return
	}
	received, err := h.LarkAPIClient.GetMessage(ctx, creds, t.delivery.ChannelMessageID.String)
	if err != nil {
		writeError(w, 503, "暂时无法核实飞书原消息，请勿报告已确认")
		return
	}
	if len(received) != 1 {
		writeError(w, 403, errBranchChat.Error())
		return
	}
	m := received[0]
	if m.MessageID != t.delivery.ChannelMessageID.String || m.Deleted || m.UpperMessageID != "" || m.SenderType != "user" || m.SenderID != t.delivery.ChannelSenderID.String {
		writeError(w, 403, errBranchChat.Error())
		return
	}
	text, ok := lark.OwnSelectionText(m.MessageType, m.Content)
	if !ok || !strings.Contains(text, in.Fragment) {
		writeError(w, 409, "这不是本人在本轮直接发出的选择；请先询问本人")
		return
	}
	e := &externalissue.FeishuBranchEvidence{UserText: text, IssueKey: in.Issue, Fragment: in.Fragment, SourceTaskID: t.delivery.TaskID}
	if more {
		// Independently inspect an explicitly named Bug outside the bounded
		// discovery page. Keep more=true: never infer global uniqueness here.
		options, _, err = h.branchChatOptions(ctx, t, in.Issue)
		if err != nil {
			branchError(w, err)
			return
		}
	}
	// An implicit issue must be the only matching choice, or be anchored to
	// the bot's actual preceding offer. Do not pick the most recent Bug.
	if !branchChatScopeMatches(options, more, t.ws.IssuePrefix, in.Repo, in.Branch, *e) || !strings.Contains(in.Branch, in.Fragment) {
		e.PriorOffer = h.priorBranchOffer(ctx, t, creds, m)
	}
	e.AmbiguousScope = !branchChatScopeMatches(options, more, t.ws.IssuePrefix, in.Repo, in.Branch, *e)
	verb := "确认分支"
	if in.Replace {
		verb = "更换分支"
	}
	reply, err := h.recordFeishuBranch(ctx, engine.ResolvedInstallation{ID: t.inst.ID, WorkspaceID: t.ws.ID, AgentID: t.inst.AgentID, Active: true}, t.user, m.MessageID, t.ws, int32(num), verb, strings.TrimSpace(in.Repo+" "+in.Branch), e)
	if err != nil {
		if errors.Is(err, errBranchChat) {
			writeError(w, 409, "对话已更新，请根据最新的本人消息重新判断")
			return
		}
		branchError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"confirmed": true, "reply": reply})
}
