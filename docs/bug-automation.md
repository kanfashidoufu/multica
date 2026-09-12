# Bug 自动化实现说明

本轮基于当前 `feat_bug_auto_fix` 分支能力整理贮藏改动，改动集中在 Syndra
Bug importer、内置 skill、脚本和测试。没有新增数据库迁移，也没有修改通用
任务状态机、IssueService、TaskService、守护进程 checkout、前端或自动化引擎。

## 运行链路

Syndra 导入 → 王宁精确匹配与唯一智能体选择 → 写入证据及交付验收 → 智能体运行
→ 确认版本分支 → 隔离修复和验证 → 合并版本分支 → 推进 `test` → 远端包含性验证 → `in_review`。

负责人或智能体不确定时保留成员处理。版本分支不能确认时，由当前人工指派人在任务
评论中明确确认；方案和合并不增加例行确认，但仓库保护规则仍然生效。

## 最小改动点

| 位置 | 职责 |
| --- | --- |
| `server/internal/externalissue/importer.go` | 复用当前 Bug 工作区、成员解析及镜像更新；仅新建流程接入派发；对已接入试运行的任务保留交付状态 |
| `server/internal/externalissue/syndra_bug_agent_routing.go` | 王宁试运行限制、唯一可执行智能体选择、成员责任归属、入队失败恢复及局部同步规则 |
| `server/internal/service/builtin_skills/multica-fixing-syndra-bugs/` | 分支证据、人工介入、工作恢复、最小修复、版本集成、`test` 推进和交付验证 |
| `server/internal/service/syndra_bug_skill_test.go` | 临时 Git 远端与模拟 GitHub CLI 的可执行验证；不调用真实智能体或实际 PR |
| `server/internal/externalissue/*test.go` | 试运行范围、原有成员回退、责任人、重复同步及失败恢复 |

内部标记 `multica_bug_automation` 由 importer 设置，源系统不能通过 metadata
开启它。普通同步逻辑不变；已接入的任务保留 Multica 状态，Syndra 原状态仍写入
原有 `bug_status` 字段。不会根据旧任务的名字或当前负责人追补启动历史任务。
试运行成员通过 `MULTICA_EXTERNAL_BUG_AUTOMATION_ASSIGNEES` 配置为逗号分隔的精确姓名；
未配置时保持默认名单 `王宁`，因此扩展测试人员只需改部署环境变量，不需要修改 importer 或 Skill。

## 当前 Multica 能力依据

- `server/internal/service/builtin_skills/multica-platform/references/issues.md`：PR 关联与关闭意图分离，PR 状态枚举及 CI 快照；任务完成和合并证据是不同事实。
- `server/internal/service/builtin_skills/multica-platform/references/projects.md`：项目资源及本地 worktree 模式，按对话保留修复分支。
- `server/internal/daemon/execenv/runtime_config_sections.go`：任务交付进入 `in_review`，`done` 保留人工；CI 结果属于显式验收时允许前台等待。本轮仅在试运行任务描述里添加这项验收，不改运行时规则。
- `server/internal/daemon/prompt.go`：当前任务、根评论摘要、相关线程和触发回复的读取与恢复。
- `server/internal/daemon/repocache/cache.go`：checkout 可能在 fetch 失败后使用缓存；重复 checkout 可能清理工作。因此分支发现查实际远端，继续运行时保护已有工作。
- `server/internal/service/task.go`：`EnqueueTaskForIssueByActor` 保持原负责成员为运行责任人，无需旧 handoff 接口或自建队列。
- [GitHub CLI 合并命令](https://cli.github.com/manual/gh_pr_merge)：`--match-head-commit` 限制请求的 PR head；队列接纳或启用 auto-merge 不是合并完成。

这些开发证据留在仓库文档，不随 skill 下发，避免让目标仓库的智能体查找不存在的
Multica 源码路径。skill 的描述和入口长度遵守当前内置 skill 模板限制。

## 分支和交付判定

有效人工确认或明确版本映射可以复用。版本名匹配只提供候选，不能证明发布意图。
版本、仓库或确认决定发生变化时必须重新确认。跨仓库修复逐仓库核对，不用一条
PR 的关闭事件代表整个 Bug 已交付。

`find-version-branches.sh` 使用 `ls-remote --heads`，拒绝多版本歧义并避免数字、
点分段前缀误命中。`verify-version-delivery.sh` 在 GitHub 实际合并后读取 base、
head、合并提交，核对 PR 属于 checkout 的实际远端仓库，精确 fetch 版本分支，并验证 merge commit 的祖先关系。`verify-test-promotion.sh` 再精确 fetch `test`，确认版本分支 tip 已进入 `test`。它支持
squash 后原 PR head 不在目标历史中、以及合并后版本分支继续前进的情况。

## 边界

- 这是 importer 的试运行规则和智能体执行合同，不是平台级禁止人工更改状态或 PR base 的硬约束。
- GitHub 有随 skill 下发的交付验证脚本。其他 forge 复用已有工具取得相同证据；工具不足时转人工。
- 验证失败、版本分支或 `test` 推进冲突无法安全解决、CI 或合并队列无法在当前运行中完成时，任务保留阻塞并通知当前人工指派人；没有引入新的后台轮询服务。
- 源 Bug 改负责人或版本不会取消已经运行的通用任务；skill 在恢复与合并前复验范围和分支，发现变化即停止。
- 贮藏中的无关 `apps/web/next-env.d.ts` 修改保留；本轮没有改动它。
