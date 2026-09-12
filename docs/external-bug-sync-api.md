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

Bug 工作区不接受请求 Query 或 body 覆盖。`workspace_id` 和 `assignee_user_id` 参数不参与同步逻辑。

## 请求体

Multica 当前支持 `syndra.multica.version_bug.webhook.v1` 结构，按 `items[]` 逐条 upsert。正式 webhook body 应直接发送 `payload` 对象；为了本地验证方便，Multica 也兼容 Syndra debug-push 返回的 `{data:{payload:{...}}}` 包装结构。

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
| `items[].creator/owner/assignee/solver` | 写入 issue metadata；创建 issue 时使用 `assignee.name` 精确匹配 Bug 工作区中的唯一 Multica 用户，并把该用户作为 assignee 和 creator；也兼容 `bug_detail.assignee.name`。负责人名称缺失、匹配不到或不唯一时，统一回退到 Bug 工作区的 owner（工作区创建人）；不使用 `mate_id`，也不读取默认指派人环境变量。试运行期间，仅当推送负责人被精确解析为王宁时，才继续尝试转交给其唯一可执行的个人智能体 |
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

试运行只处理 **Syndra 明确指派给王宁** 的新 Bug，接入点保持在 Syndra importer：

1. `assignee.name`（兼容 `bug_detail.assignee.name`）必须精确匹配 Bug 工作区中唯一的王宁成员。名称缺失、不匹配或重名时仍回退到工作区 owner，但不会进入自动化。
2. 该成员必须恰好拥有一个未归档、已绑定运行时的智能体；零个或多个候选都保留成员指派。
3. 仅 `todo` / `in_progress` 的新任务可自动派发。其他成员、其他来源和已有任务不会因此新建运行。
4. 先创建成员任务并写完 Syndra 证据和自动化验收要求，再改派智能体，复用 `EnqueueTaskForIssueByActor` 创建运行。成员仍是创建人、订阅人及运行责任人。入队失败会尝试恢复成员指派并记录错误。

进入试运行时会由 importer 写入内部布尔标记 `multica_bug_automation=true`；
该字段不接受 Syndra payload 设置。新建时 metadata 已满则保留成员处理；已接入任务的后续同步若无法保留内部标记，会返回错误并保留现状，不越过字段上限。
描述追加验收要求：明确版本分支、取得验证和 CI 结果；验证通过后实际合入版本分支并推送，再合入 `test` 并推送；验证失败时将当前快照提交到版本分支并 block 任务，通知人工指派人介入。
这使当前运行时可以按任务的验收要求取得 CI 结果，而不会把“已开 PR，CI 运行中”当成交付。

后续 upsert 仍更新源描述、优先级和 Syndra metadata，但已进入试运行的任务保留
Multica 当前状态和内部标记，避免源系统的 `resolved` 把未合入版本的任务改成 `done`。
普通任务保持原有 status/metadata 镜像语义。两类任务都保留 Multica 当前负责人，
不会因为重复同步重复启动。源版本或负责人变化后，智能体在下一轮和合并前重新核对，
不沿用失效分支决定。

分支与交付流程由内置 `multica-fixing-syndra-bugs` skill 和两个校验脚本承接：

- 以“Bug、版本、仓库、远端分支”为一组确认信息。当前创建人的有效回复，或明确关联该版本和仓库的项目映射，可以复用；普通项目 `ref`、默认分支提示和环境分支不能作为依据。
- 模糊匹配只列候选。即使只有一个候选，没有确定证据也必须请王宁确认。多候选、缺失版本、信息冲突、远端不可访问或分支不存在均进入 `blocked`。
- 读取最新远端分支，完整区分 `2.91.56`、`2.91.560`、`2.91.56.1`；不按 `_wn`、`_merge` 后缀猜测发布目标。
- 复用托管 checkout 或 `local_directory.execution_mode=worktree`。每个受影响仓库使用独立修复分支和 PR，恢复运行时先读取已有讨论、分支和 PR，保护前轮工作。
- 验证通过后按仓库规则合并并推送确认的版本分支，再把版本分支合入远端 `test` 并推送。GitHub 使用已验证 head 约束合并请求；进入队列或启用自动合并不算完成。脚本核对真实 merged 状态、PR base/head，并确认最新远端版本分支包含合并提交；`test` 还必须有独立的合并提交和远端包含性证据。其他 forge 必须取得同等证据。
- 验证失败时先把当前修复快照合入并推送版本分支，任务保持 `blocked`，评论中通知当前人工指派人及失败检查；不推进 `test`，也不进入 `in_review`。
- 所有受影响仓库都完成合入后，才交给创建人 review，任务进入 `in_review`。PR 使用标题关联任务，不设置可能提前自动关闭任务的 `Fixes/Closes/Resolves`；`done` 由人工验收。

`waiting_on` / `blocked_reason` 仅作临时检索游标，后续同步可能覆盖它们。
分支决定保存在评论中，恢复时读取历史证据，不因游标丢失重复询问，也不据此猜测授权。
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
