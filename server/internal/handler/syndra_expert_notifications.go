package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	enterpriseLark "github.com/multica-ai/multica/server/internal/enterprise/lark"
	"github.com/multica-ai/multica/server/internal/externalissue"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/notifications"
	"github.com/multica-ai/multica/server/internal/projectaccess"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type SyndraExpertNotifications struct{ Handler *Handler }

func (c SyndraExpertNotifications) Name() string              { return "syndra_expert_feishu" }
func (c SyndraExpertNotifications) PreferenceChannel() string { return "lark" }
func (c SyndraExpertNotifications) MatchesIssue(ctx context.Context, workspace, id pgtype.UUID) (bool, error) {
	q := c.Handler.Queries
	ws, err := q.GetWorkspace(ctx, workspace)
	if err != nil || ws.Slug != externalissue.ExpertWorkspace {
		return false, err
	}
	bug, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspace, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && externalissue.IsSyndraBug(bug), err
}

func (c SyndraExpertNotifications) Send(ctx context.Context, msg notifications.NotificationMessage) error {
	h := c.Handler
	ws, user := parseUUID(msg.WorkspaceID), parseUUID(msg.RecipientUserID)
	id, err := util.ParseUUID(msg.IssueID)
	if err != nil {
		return errors.New("notification no longer references an issue")
	}
	ok, err := c.MatchesIssue(ctx, ws, id)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("Syndra bug no longer in expert workspace")
	}
	bug, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: ws, ID: id})
	if err != nil {
		return err
	}
	member, err := h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{WorkspaceID: ws, UserID: user})
	if err != nil {
		return err
	}
	visible, err := projectaccess.ForMember(ctx, h.Queries, member)
	if err != nil {
		return err
	}
	if !visible.CanSeeProject(bug.ProjectID) {
		return errors.New("recipient cannot access bug")
	}
	installs, err := h.LarkInstallations.ListByWorkspace(ctx, ws)
	if err != nil {
		return err
	}
	var selected lark.Installation
	for _, inst := range installs {
		if inst.Status != string(lark.InstallationActive) {
			continue
		}
		agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: ws, ID: inst.AgentID})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		ok, err := externalissue.IsWorkspaceBugExpert(ctx, h.Queries, agent, ws)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if selected.ID.Valid {
			return errors.New("ambiguous active Feishu bug expert installation")
		}
		selected = inst
	}
	if !selected.ID.Valid {
		return errors.New("no active Feishu bug expert installation")
	}
	binding, err := h.Queries.FindChannelBindingForMemberOnAgentInstallation(ctx, db.FindChannelBindingForMemberOnAgentInstallationParams{
		WorkspaceID: ws, MulticaUserID: user, ChannelType: string(channel.TypeFeishu), AgentID: selected.AgentID,
	})
	idType, recipient := "open_id", binding.ChannelUserID
	if errors.Is(err, pgx.ErrNoRows) && selected.TenantKey.String != "" {
		// The login app's open_id is NOT portable. A verified tenant user_id
		// can receive the first proactive message before the member binds this bot.
		idType = "user_id"
		err = h.DB.QueryRow(ctx, `SELECT min(external_user_id) FROM user_external_identity
			WHERE user_id=$1 AND provider='lark' AND tenant_key=$2 AND external_user_id<>''
			HAVING count(DISTINCT external_user_id)=1`, user, selected.TenantKey.String).Scan(&recipient)
	} else if err == nil && binding.InstallationID != selected.ID {
		return errors.New("recipient binding belongs to a different Feishu installation")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Scan-installed bots can lack a tenant_key. A unique union_id is
		// addressable across apps of the same developer; Feishu rejects it
		// outside that scope. Never substitute the login app's open_id.
		idType = "union_id"
		err = h.DB.QueryRow(ctx, `SELECT min(union_id) FROM user_external_identity
			WHERE user_id=$1 AND provider='lark' AND union_id<>'' AND ($2::text='' OR tenant_key=$2)
			HAVING count(DISTINCT union_id)=1`, user, selected.TenantKey.String).Scan(&recipient)
	}
	if err != nil || recipient == "" {
		return fmt.Errorf("missing recipient binding or verified tenant user ID: %v", err)
	}
	creds, err := selected.Credentials(h.LarkInstallations)
	if err != nil {
		return err
	}
	_, err = h.LarkAPIClient.SendTextMessage(ctx, lark.SendTextParams{InstallationID: creds, RecipientIDType: idType, RecipientID: recipient, Text: enterpriseLark.NotificationText(msg), IdempotencyKey: msg.InboxItemID})
	return err
}
