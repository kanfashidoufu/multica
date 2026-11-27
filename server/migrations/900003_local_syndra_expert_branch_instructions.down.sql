-- Replace only the installed expert's known obsolete sentence; retain local edits.
UPDATE agent a
SET instructions = replace(a.instructions,
    '本人可在原 Bug 评论「默认 [仓库] 分支名」，或私聊本应用发送「确认分支 原Bug编号 [仓库] 分支名」。不要求关闭确认单或手动恢复。只以服务端分支回执为准，不以智能体转述代替本人。已有个人版本分支选择继续复用。修复时只用 SSH 拉取代码，保留各 Bug 的独立 worktree。',
    '本人分支确认沿用 Multica 原确认单，已有个人版本分支选择继续复用。'), updated_at = now()
FROM workspace w
WHERE w.id = a.workspace_id AND w.slug = 'qtb-fe'
  AND a.name = 'bug修复专家' AND a.kind = 'user' AND a.archived_at IS NULL
  AND strpos(a.instructions, '本人可在原 Bug 评论「默认 [仓库] 分支名」，或私聊本应用发送「确认分支 原Bug编号 [仓库] 分支名」。不要求关闭确认单或手动恢复。只以服务端分支回执为准，不以智能体转述代替本人。已有个人版本分支选择继续复用。修复时只用 SSH 拉取代码，保留各 Bug 的独立 worktree。') > 0
  AND EXISTS (SELECT 1 FROM channel_installation c WHERE c.workspace_id = w.id
              AND c.agent_id = a.id AND c.channel_type = 'feishu' AND c.status = 'active');
