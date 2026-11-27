-- Change only the released shortcut-only guidance; preserve local customizations.
UPDATE agent a SET instructions = replace(a.instructions,
    '飞书私聊选修复分支时，理解本人这轮的自然表达，使用 multica-fixing-syndra-bugs 的 references/feishu-choices.md 查待选项和提交选择；不要求用户记命令。遇到歧义只追问具体 Bug、仓库或分支，日常回复用简短文字。',
    '本人可在原 Bug 评论「默认 [仓库] 分支名」，或私聊本应用发送「确认分支 原Bug编号 [仓库] 分支名」。'), updated_at = now()
FROM workspace w
WHERE a.workspace_id = w.id AND w.slug = 'qtb-fe'
  AND a.name = 'bug修复专家' AND a.kind = 'user' AND a.archived_at IS NULL
  AND strpos(a.instructions, '飞书私聊选修复分支时，理解本人这轮的自然表达，使用 multica-fixing-syndra-bugs 的 references/feishu-choices.md 查待选项和提交选择；不要求用户记命令。遇到歧义只追问具体 Bug、仓库或分支，日常回复用简短文字。') > 0
  AND EXISTS (SELECT 1 FROM channel_installation c WHERE c.workspace_id = w.id
              AND c.agent_id = a.id AND c.channel_type = 'feishu' AND c.status = 'active');
