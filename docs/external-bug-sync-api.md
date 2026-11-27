# 内部 Bug 平台同步接口文档

本文档用于内部 Bug 管理平台把 Syndra `version_bug` webhook 同步为 Multica issue。

## 接口

`POST https://<multica-api-host>/api/webhooks/external-issues?sync_type=bug`

必须携带 `sync_type=bug`。飞书小需求导入已下线；缺少或传入不支持的 `sync_type` 会返回 `400`。Syndra 小需求请求请使用 `sync_type=requirement`，详见 [Syndra 小需求同步接口](./syndra-requirement-sync-api.md)。

## 鉴权与配置

请求头：

```http
Authorization: Bearer <MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN>
Content-Type: application/json
```

Multica 侧需要配置：

| 环境变量 | 说明 |
|---|---|
| `MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN` | webhook 鉴权 token |
| `MULTICA_EXTERNAL_BUG_WORKSPACE_ID` | Bug 固定创建到的 workspace UUID |
| `MULTICA_EXTERNAL_BUG_AUTOMATION_ASSIGNEES` | 试运行自动化允许的 Syndra Bug 指派人姓名，逗号分隔，按 `assignee.name` 精确匹配；未配置时默认为 `王宁` |

Bug 工作区不接受请求 Query 或 body 覆盖。`workspace_id` 和 `assignee_user_id` 参数不参与同步逻辑。

### 按飞书身份免邀请入组（默认开启）

部署配套版本的 Syndra 与 Multica 后，Bug 同步默认尝试免邀请入组，仅作用于原有的 Bug 工作区，不需要新增开关、回调地址或租户配置。两端仍需具备原有同步凭据、可用的飞书应用及通讯录权限。

| 位置 | 如何核验 |
| --- | --- |
| Syndra | 复用当前同步来源环境，回查 Bug 的实际负责人。使用已有飞书应用读取其真实租户，并按手机号取员工 `user_id`。 |
| Multica | 按 `prod / pre / test / dev` 选择对应环境的固定 Syndra API，未知环境不开户。回查请求与结果使用现有同步密钥按本单签名，不向回查地址转发 webhook 原始凭据。用已启用的飞书登录应用独立取得真实租户，与 Syndra 的结果比对；已配置的租户限制仍生效。 |

1. 按 webhook 声明的协议处理：v1 保留工作区唯一姓名匹配和 `mate_id` 核验；生产 v2 一律用 `assignee.syndra_user_id` 回查实际指派。该字段必须是正 64 位整数的十进制字符串，不能用旧 `mate_id` 或同名账号代替。
2. v2 由 Syndra 在配置的租户内用 `assigned_user_id → qts_user.id` 查当前在职且准入的员工，并使用生产登录应用绑定的身份取得其飞书 `user_id`。请求和回执使用 v2 专用签名域，v1 签名不能混用。Multica 再用自己的登录应用独立核实租户和员工状态；无需配置新开关。
3. 按已绑定飞书身份或飞书目录返回的公司 `enterprise_email` 复用全局账号；双方身份冲突不合并。无邮箱时，新账号使用原登录流程的 `@lark.local` 占位地址；`bug_assignee_email_candidate` 是姓名全拼生成的**未核实建议**，不参与账号匹配、登录、邀请或发送邮件。
4. 账号、身份绑定和普通 `member` 入组在同一事务提交，不创建邀请或登录会话。已有同范围普通成员邀请可结清；其他角色、项目邀请、注册或成员额度受限、云端托管席位等情况不绕过原生准入，维持 owner 兜底。处理原因或补齐授权后重推同一 Bug 即可。首次访问仍需本人飞书登录。

### 本人登录后补全资料

Syndra 同步不会用拼音替换 `@lark.local`。本人通过飞书 OAuth 登录后，Multica 先按已绑定的飞书身份找到原账号，再使用本次登录返回的 `enterprise_email` 补全仍为占位值的主邮箱。已有真实邮箱不覆盖，普通联系邮箱 `email` 不能单独用于补全。企业邮箱缺失、无效或已被其他账号使用时保留占位邮箱，不合并账号，也不阻塞本人通过飞书登录。

登录时仍用非空姓名和头像刷新原账号资料；飞书没返回头像时保留已有头像。这个过程不重新开户、发邀请或变更 Issue 指派，没有新增配置或数据库迁移。

### Multica 完成 → Syndra 已解决

仅对配置的 Bug 工作区、通过原始导入标识核实的 Syndra Bug 生效，无需额外环境变量。Issue 从非完成状态进入 `done`（包括工作区自定义的完成类状态）时，在状态事务里记录一次待回写事件。新建即完成、切到审核或取消、完成类之间移动、重复保存以及 Syndra 正向镜像均不触发；不回灌历史完成单。

备注内容在切换时固定：优先选择本 Issue 最新一条以 **处理结果：** 或 **修复结果：** 开头的评论，只取其第一段；否则取最近一次成功任务的最终回复。两者都没有时写“Multica 已确认完成，未填写处理详情。”最多 4000 个字符，不取普通聊天、提示词或工具日志。人工处理时可先评论，例如：`处理结果：修正了分页漏查，已用两页数据复验。`

服务端异步调用对应来源环境的 `POST /open/multica/version-bugs/resolve`，用原有同步密钥对本次原始请求体签名，不转发 webhook token。失败按退避间隔重试，重启后可续投。Syndra 在同一事务中写状态和可见备注；已解决或已关闭的 Bug 只补备注，不改原结论。重复投递不重复写，过期 Bug 或已重新激活后的旧事件不修改。Syndra 实际转为已解决后按原链路推送新快照，此次镜像不会再触发回写。

部署顺序：先升级 Syndra 后端，再按 Multica 正常迁移流程应用本地迁移 `900000–900002`，最后启动新版 Multica 服务。回滚这些迁移会删除尚未投递的事件，应先核对队列。

## 请求体

Multica 当前支持 `syndra.multica.version_bug.webhook.v1` 和 `syndra.multica.version_bug.webhook.v2` 结构，按 `items[]` 逐条 upsert。所有岗位、所有 Bug 状态及无版本 Bug 共用此入口，不代表全部进入自愈。正式 webhook body 应直接发送 `payload` 对象；为了本地验证方便，Multica 也兼容 Syndra debug-push 返回的 `{data:{payload:{...}}}` 包装结构。

关键字段：

| 字段 | 说明 |
|---|---|
| `source` | 来源，默认按 `syndra` 处理 |
| `source_env` | 来源环境，参与幂等 key |
| `event_id` | 平台事件 ID，写入 issue metadata |
| `items[].event` | 支持 `upsert/create/update/change` 等写入事件 |
| `items[].external_key` | 幂等主键，优先使用；缺失时用 `entity_type:bug_id` |
| `items[].bug_id` | Bug 平台 ID |
| `items[].title` | 原始 Bug 标题；Multica issue 标题会自动加上 `【Bug#<bug_id>】【<version_name>】` 前缀 |
| `items[].description` | Multica issue 描述，支持将简单 HTML `<p>/<br>` 转为文本，并将 `http/https` 的 `<img src="...">` 转为 Markdown 图片 |
| `items[].status/status_name` | 普通任务映射为 Multica issue 状态；已进入自动化的任务保留本地交付状态，源状态继续写入 metadata |
| `items[].bug_level/priority` | 映射为 Multica issue 优先级 |
| `items[].bug_type_id/bug_type` | 写入 issue metadata |
| `items[].creator/owner/assignee/solver` | 人员信息写入 metadata。v1 沿用原匹配规则；v2 只按回查核实的飞书身份复用或创建账号，不按姓名或拼音邮箱合并。无法核实时由 owner 承接；自动修复仍受白名单约束。 |
| `items[].module` | 写入 issue metadata |
| `items[].resolve_solution/resolve_solution_name` | 写入 issue metadata |
| `items[].attachments/videos` | 当前记录数量到 metadata，暂不下载并绑定 Multica attachment |
| `items[].bug_detail` | 从中提取 `bug_url/source_url/version/module/creator/assignee` 等关键 primitive 字段写入 metadata |
| `items[].source_url` | 写入 metadata，便于回跳 Syndra |
| `items[].metadata` | 仅同步其中的 primitive 值到 issue metadata |

状态映射：

| Bug 状态 | Multica 状态 |
|---|---|
| `active` / `open` / `激活` | `todo` |
| `处理中` / `修复中` / `解决中` / `in progress` | `in_progress` |
| `待验证` / `review` | `in_review` |
| `resolved` / `fixed` / `closed` / `已解决` / `关闭` / `完成` | `done` |
| `blocked` / `阻塞` | `blocked` |
| `cancelled` / `取消` | `cancelled` |

优先级映射：

| Bug 严重程度 | Multica 优先级 |
|---|---|
| `P0` / `P1` | `urgent` |
| `P2` | `high` |
| `P3` | `medium` |
| `P4` / `P5` | `low` |

## Bug 自动化试运行

试运行只修复 **Syndra 明确指派给白名单成员** 的 Bug，包括重新推送、仍待修复的旧任务。名单由 `MULTICA_EXTERNAL_BUG_AUTOMATION_ASSIGNEES` 配置，使用逗号分隔的成员姓名；未配置时默认为 `王宁`：

1. `assignee.name`（兼容 `bug_detail.assignee.name`）必须精确匹配 Bug 工作区中唯一的允许名单成员。无法确认唯一允许成员时不进入自动化；免邀请入组不扩大试运行名单。
2. 在 Bug 工作区按名称精确选择 `bug修复专家`。专家须已绑定运行时、未归档，并已向整个工作区开放调用。没有唯一匹配时保留成员指派。
3. 接到新建或更新推送时，源端和本地任务仍待修复（待办或进行中），且没有排队、延后、已下发、运行中或等待本地目录的运行，才可接管。不扫描历史任务，也不抢占旧智能体的工作。
4. 改派专家、写入自愈验收说明和运行入队同时提交或回滚；重复推送不重复派发。原人工负责人仍承担分支确认和审核。

进入试运行时会由 importer 写入内部布尔标记 `multica_bug_automation=true`；
该字段不接受 Syndra payload 设置。若 metadata 已满，优先腾出有相同规范字段的冗余来源别名；仍无空间时，新建任务保留成员处理；已接入任务的后续同步若无法保留内部标记，会返回错误并保留现状，不越过字段上限。
描述追加验收要求：明确版本分支、取得验证和 CI 结果；验证通过后实际合入版本分支并推送，再合入 `test` 并推送；验证失败时将当前快照提交到版本分支并 block 任务，通知人工指派人介入。
这使当前运行时可以按任务的验收要求取得 CI 结果，而不会把“已开 PR，CI 运行中”当成交付。

**源端收尾不受白名单限制。** 收到有效的 `resolved`（已解决）或 `closed`（已关闭）推送时，对应 Multica 任务进入 `done`，同时取消它的全部在途运行并禁用等待规则。已经结束的运行保留原记录，不影响其他任务，也不会由本次镜像再触发一次反向回写。来源已结束不代表专家完成了代码合并。其他状态的推送继续保留已接入自愈任务的本地交付阶段。
普通任务同步状态，也把可访问本 Issue 的源负责人更新为实际指派人；创建人不随改派更换。未能确认或接纳成员时保留源姓名和 `bug_assignee_fallback_reason`，由工作区 owner 承接。账号补齐后重推同一个 Bug 即可恢复指派；只有实际改派才通知新成员。
已由专家接管的任务不会因重推重复创建专家运行；人工分支确认仍以最新的源负责人为准，owner 兜底不代替本人确认。带有可比较的 `updated_at` 时，较旧快照不会覆盖较新快照或停止新工作。
源版本或负责人变化后，智能体在下一轮和合并前重新核对，
不沿用失效分支决定。

分支与交付流程由内置 `multica-fixing-syndra-bugs` skill 和两个校验脚本承接：

- 先读本单例外，再读当前人工指派人的个人默认分支；按工作区、来源环境、仓库、版本 ID、成员 UUID 隔离。首次缺少默认值时共用一张确认单；本人回复后，等待该确认单的 Bug 通过原有任务队列恢复。项目映射或分支后缀不能替代个人选择。变更负责人后重新解析，有在途修改时保留原 PR 交接。
- 模糊匹配只列候选。已有有效个人选择时，即使匹配到多个分支也直接使用本人选定的分支。开发前没有决定时保持阻塞；若修复已完成，只等负责人确认目标分支，则进入审核中，确认前不合并。
- 读取最新远端分支，完整区分 `2.91.56`、`2.91.560`、`2.91.56.1`；不按 `_wn`、`_merge` 后缀猜测发布目标。
- 专家先按工作区仓库目录（URL 和描述）、Bug 模块和复现证据核实受影响的代码，再对每个仓库创建托管 checkout 或复用对应的隔离 worktree。每个受影响仓库使用独立修复分支和 PR，恢复运行时先读取已有讨论、分支和 PR，保护前轮工作。
- 验证通过后按仓库规则合并并推送确认的版本分支，再把版本分支合入远端 `test` 并推送。GitHub 使用已验证 head 约束合并请求；进入队列或启用自动合并不算完成。脚本核对真实 merged 状态、PR base/head，并确认最新远端版本分支包含合并提交；`test` 还必须有独立的合并提交和远端包含性证据。其他 forge 必须取得同等证据。
- 验证失败时先把当前修复快照合入并推送版本分支，任务保持 `blocked`，评论中通知当前人工指派人及失败检查；不推进 `test`，也不进入 `in_review`。
- 修复完成、仅待当前负责人确认或审批时，任务进入审核中（`in_review`），即使尚有合并待执行。评论写明需要确认什么、哪些分支已合入。只有所有受影响仓库的版本分支和 `test` 都取得远端证据，才能报告已交付。PR 使用标题关联任务，不设置可能提前自动关闭任务的 `Fixes/Closes/Resolves`；`done` 由人工验收。

`waiting_on` / `blocked_reason` 仅作临时检索游标，后续同步可能覆盖它们。
个人默认值以共享确认单及本人回复为准，原 Bug 评论只保存本单例外。部署时须同步升级服务端与执行端 CLI；缺少分支命令时保持阻塞，不回退到其他人的确认。
CI 或合并队列尚未完成时明确保留阻塞并给出恢复动作；当前流程不创建额外定时任务，
也不假设任务退出后会自动被 CI 完成事件唤醒。

实现边界与验证说明见 [Bug 自动化实现说明](bug-automation.md)。

## 示例

```bash
curl -X POST 'https://<multica-api-host>/api/webhooks/external-issues?sync_type=bug' \
  -H 'Authorization: Bearer <token>' \
  -H 'Content-Type: application/json' \
  -d '{
    "schema_version": "syndra.multica.version_bug.webhook.v1",
    "event_type": "version_bug.changed",
    "event_id": "syndra:local:version_bug:frontend_debug:1782787825475",
    "scene": "frontend_debug",
    "source": "syndra",
    "source_env": "local",
    "sent_at": "2026-06-30T10:50:25+08:00",
    "item_count": 1,
    "item_ids": "1081",
    "items": [{
      "event": "upsert",
      "entity_type": "version_bug",
      "external_key": "syndra:local:version_bug:1081",
      "bug_id": 1081,
      "version_id": 163,
      "version_name": "v2.91.56-企业一体化项目看板",
      "role": "frontend",
      "title": "【生成报告】iOS 14.6/16.1 白屏",
      "description": "版本：v2.91.56<br><p>[步骤]</p><p>打开生成报告后白屏</p>",
      "priority": "一般",
      "bug_level": "P3",
      "bug_type_id": 8,
      "bug_type": "前端-开发代码",
      "status": "active",
      "status_name": "激活",
      "resolve_solution": null,
      "resolve_solution_name": "",
      "creator": {"mate_id": 2076, "name": "李景华"},
      "assignee": {"mate_id": 2401, "name": "刘鹏", "dept_name": "研发中心/技术部/前端组"},
      "module": {"module_id": 91, "module_name": "统计"},
      "attachments": [],
      "videos": [],
      "bug_detail": {
        "bug_id": 1081,
        "title": "【生成报告】iOS 14.6/16.1 白屏",
        "description": "<p>[步骤]</p><p>打开生成报告后白屏</p>",
        "bug_level": "P3",
        "priority": "一般",
        "bug_type_id": 8,
        "bug_type_name": "前端-开发代码",
        "status": "active",
        "status_name": "激活",
        "module": {"module_id": 91, "module_name": "统计"},
        "version": {
          "version_id": 163,
          "version_name": "v2.91.56-企业一体化项目看板",
          "version_type": 1,
          "version_status": 8
        },
        "creator": {"mate_id": 2076, "name": "李景华"},
        "assignee": {"mate_id": 2401, "name": "刘鹏", "dept_name": "研发中心/技术部/前端组"},
        "bug_url": "https://zentao.lggj.work/zentao/bug-view-29593.html",
        "source_url": "http://192.168.215.31:9001/#/qms/bugCenter/bugManager?bugId=1081",
        "attachments": [],
        "videos": []
      },
      "labels": ["syndra", "frontend", "bug", "P3"],
      "source_url": "http://192.168.215.31:9001/#/qms/bugCenter/bugManager?bugId=1081",
      "metadata": {"syndra_role": "frontend"}
    }]
  }'
```

## 成功响应

首次创建返回 `201 Created`；已存在来源记录的重复推送返回 `200 OK` 并更新同一条 issue。

```json
{
  "status": "synced",
  "sync_type": "bug",
  "provider": "syndra",
  "item_count": 1,
  "synced": 1,
  "ignored": 0,
  "existing": false,
  "source_record_id": "syndra:local:version_bug:1081",
  "external_key": "syndra:local:version_bug:1081",
  "bug_id": 1081,
  "issue": {
    "id": "<issue uuid>",
    "identifier": "MUL-123",
    "title": "【Bug#1081】【v2.91.56-企业一体化项目看板】【生成报告】iOS 14.6/16.1 白屏",
    "status": "todo",
    "priority": "medium"
  }
}
```

批量 `items` 会同时返回 `items[]`，每项包含 `status/existing/source_record_id/external_key/bug_id/issue`。

## 错误响应

| 状态码 | 场景 |
|---:|---|
| `401` | `Authorization` token 缺失或错误 |
| `503` | Multica 未配置 webhook token、`MULTICA_EXTERNAL_BUG_WORKSPACE_ID`，或目标工作区没有可用的 owner |
| `400` | JSON 请求体无效、workspace/record/title 缺失 |
