# Bug 自动化实现说明

本轮基于当前 `feat_bug_auto_fix` 分支能力整理贮藏改动，改动集中在 Syndra
Bug importer、专家飞书集成、内置 Skill、脚本和测试。分支选择复用原有数据结构；
本地迁移仅替换已安装专家指令中「沿用原确认单」的旧句子，不改数据库结构、守护进程 checkout 或桌面端。

## 运行链路

Syndra 导入 → 名单成员匹配与工作区公用专家选择 → 写入证据及交付验收 → 智能体运行
→ 确认版本分支 → 隔离修复和验证 → 合并版本分支 → 推进 `test` → 远端包含性验证 → `in_review`。

负责人或智能体不确定时保留成员处理。版本分支不能确认时，由当前人工指派人在任务
评论中明确确认；方案和合并不增加例行确认。代码已改完且只等当前负责人确认时，任务进入审核中（`in_review`）；验证失败、冲突待修等技术阻塞仍用 `blocked`。自愈任务可直接使用 Git 提交，无需 QTB 本地提交门禁回执；本地验证、必需 CI 和仓库保护规则保持不变。

## 最小改动点

| 位置 | 职责 |
| --- | --- |
| `server/internal/externalissue/importer.go` | 新建及更新推送共用白名单派发和状态同步规则 |
| `server/internal/externalissue/syndra_bug_agent_routing.go` | 试运行名单、工作区公用 bug修复专家选择、成员责任归属、入队失败恢复及局部同步规则 |
| `server/internal/service/builtin_skills/multica-fixing-syndra-bugs/` | 分支证据、人工介入、工作恢复、最小修复、版本集成、`test` 推进和交付验证 |
| `server/internal/service/syndra_bug_skill_test.go` | 临时 Git 远端与模拟 GitHub CLI 的可执行验证；不调用真实智能体或实际 PR |
| `server/internal/externalissue/*test.go` | 试运行范围、原有成员回退、责任人、重复同步及失败恢复 |

内部标记 `multica_bug_automation` 由 importer 设置，源系统不能通过 metadata
开启它。Syndra 原状态写入 `bug_status`。只有再次收到推送，仍待修复且无人执行的旧 Bug 才按白名单接管；不批量重跑历史。源端明确已解决或已关闭时，无论是否自愈，Multica 都将对应任务设为已完成并停止其在途运行，其余任务不受影响。
试运行成员通过 `MULTICA_EXTERNAL_BUG_AUTOMATION_ASSIGNEES` 配置为逗号分隔的精确姓名；
未配置时保持默认名单 `王宁`，因此扩展测试人员只需改部署环境变量，不需要修改 importer 或 Skill。

## 当前 Multica 能力依据

- `server/internal/service/builtin_skills/multica-platform/references/issues.md`：PR 关联与关闭意图分离，PR 状态枚举及 CI 快照；任务完成和合并证据是不同事实。
- `server/internal/service/builtin_skills/multica-platform/references/projects.md`：项目资源及本地 worktree 模式，按对话保留修复分支。
- `server/internal/daemon/execenv/runtime_config_sections.go`：任务交付进入 `in_review`，`done` 保留人工；CI 结果属于显式验收时允许前台等待。本轮仅在试运行任务描述里添加这项验收，不改运行时规则。
- `server/internal/daemon/prompt.go`：当前任务、根评论摘要、相关线程和触发回复的读取与恢复。
- `server/internal/daemon/repocache/cache.go`：checkout 可能在 fetch 失败后使用缓存；重复 checkout 可能清理工作。因此分支发现查实际远端，继续运行时保护已有工作。
- `server/internal/service/task.go`：改派与入队在同一事务提交，复用现有队列和人工责任归属。
- [GitHub CLI 合并命令](https://cli.github.com/manual/gh_pr_merge)：`--match-head-commit` 限制请求的 PR head；队列接纳或启用 auto-merge 不是合并完成。

这些开发证据留在仓库文档，不随 skill 下发，避免让目标仓库的智能体查找不存在的
Multica 源码路径。skill 的描述和入口长度遵守当前内置 skill 模板限制。

## 分支和交付判定

通过 `multica issue syndra-branch <任务ID> --repo <仓库URL>` 查询个人默认分支。确认单复用原有任务唯一来源约束，无需新增表或迁移。工作区、Syndra 来源环境、仓库、版本 ID、当前人工指派人的成员 UUID 共同确定一张确认单。同一人并行处理的 Bug 共用一次确认，各自通过 Multica 的一次性条件唤醒继续运行。不同人的确认单互不影响。

在 `qtb-fe`，负责人在原 Bug 评论 `默认 release/v3.12.0_wn`，或绑定后私聊「黑默丁格·Bug 修复专家」发送 `确认分支 QTB-1974 release/v3.12.0_wn`。多个仓库等待时，在分支名前加通知里显示的完整仓库名。分支等待由系统自动结束，不需要手动改状态；后台仍复用一张不指派给人的共享记录。更换失效分支用通知中的 `更换分支`（评论用 `更换默认`）。旧确认单仍支持本人直接回复；在原 Bug 的分支说明下回复 `仅本单 release/v3.12.0_hotfix` 只覆盖该 Bug。

专家应用额外发送该工作区 Syndra Bug 的飞书通知（含处理进度、阻塞和审核），原统一推送保留。按安装绑定的成员身份发私信；没有绑定时可用同一飞书租户的已核实 user_id，或同一开发者应用可识别的唯一 union_id，绝不复用另一个应用的 open_id。收件人仍须在飞书应用的可用范围内。普通聊天、其他工作区和非 Syndra 任务不走此路由。

修复运行时一律使用 SSH。Skill 校验已登记的仓库 URL，为各 Bug 建独立 clone 和 worktree，不改工作区登记、共享 origin 或全局 Git 配置；HTTPS 标准地址转换成 SSH，不能确定 SSH 端口或缺少 key/权限时保留现场并通知负责人。
负责人、版本或来源环境变化时，导入器撤销旧分支等待；阻塞中或审核中的任务在新负责人可解析时，会分别发送接手或审核通知。恢复时重新解析；已有修复分支或 PR 的目标发生变化时，保留工作并人工交接。跨仓库修复逐仓库核对，不用一条
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
