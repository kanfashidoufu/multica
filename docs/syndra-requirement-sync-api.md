# Syndra 小需求同步接口

本文档用于 Syndra 通过普通 HTTP 请求把小需求同步为 Multica Issue。接口会记录完整原始请求体，按 `id` 幂等创建或刷新同一条 Issue，并自动把 Issue 指派给 `executor_id` 对应的 Multica Agent 开始开发。

## 联调环境

| 环境 | 请求地址 | Bearer Token |
|---|---|---|
| 本地联调 | `https://kiwi-unequal-automated.ngrok-free.dev/api/webhooks/external-issues?sync_type=requirement` | `Widp-Pu0BJRfI6-IqU1LYJXg6vRzy5koF9B8W_VdSLac9LXQgIV3YZ0kpJXO9Fcl` |
| 线上 | `https://multica-api.lggj.net/api/webhooks/external-issues?sync_type=requirement` | `HmLFO9xGsVa0tbSuJMbAaUOArgvNPGFXvDUad4TF3T0euCFY3v_rX7zpJILsCKI6` |

`sync_type=requirement` 是必填 Query 参数。原飞书多维表格小需求导入已下线，不再接受 `app_token`、`table_id`、`record_id` 等参数。

> 本文档包含有效联调凭据，请控制传播范围。Token 变更后需要同步更新 Syndra 配置。

## 鉴权和请求格式

请求必须使用 `POST`，并携带以下请求头：

```http
Authorization: Bearer <MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN>
Content-Type: application/json
```

Syndra 应直接把 JSON 对象放入 HTTP body。不要转换成表单，不要把 JSON 编码成字符串，不要通过文件上传。Multica 接受未声明的扩展字段，并会把收到的完整 body 写入服务端日志，便于后续排查。

## Multica 配置

| 环境变量 | 说明 |
|---|---|
| `MULTICA_EXTERNAL_ISSUE_WEBHOOK_TOKEN` | Bug 和小需求 webhook 共用的 Bearer Token |
| `MULTICA_EXTERNAL_REQUIREMENT_WORKSPACE_ID` | 小需求固定创建到的 workspace UUID |

小需求工作区与 Bug 工作区相互独立。小需求接口不接受 `workspace_id` 覆盖。小需求不再使用默认指派人环境变量；负责人和执行 Agent 都从本次 Syndra 请求解析。

## 请求体

### 当前必填字段

| 字段 | 类型 | Multica 用途 |
|---|---|---|
| `id` | string | 外部幂等键。同一个工作区内，相同 `id` 只对应一条 Issue |
| `title` | string | Issue 标题中的需求名称部分 |
| `execution_prompt` | string | Issue 正文中的主体内容。应传入完整的自动开发执行 Prompt |
| `executor_id` | UUID string | Issue 的 `assignee_id`，必须是小需求工作区中的 Multica Agent ID |
| `owner_id` | string | Syndra 负责人 ID，写入 Issue metadata 供追踪 |
| `product_iteration_id` | integer | Syndra 需求 ID，写入 Issue metadata |
| `project_record_url` | string | 项目记录链接，写入 Issue metadata |

### 当前映射

| Syndra 字段 | Multica 字段或行为 |
|---|---|
| `title`、`id`、`dispatch_key/current_attempt` | 组装 `issue.title` |
| `product_iteration_id`、`project_record_url`、`development_role`、`execution_prompt`、`lane_id`、`lane_type`、`provider_run_id` 等 | 组装 `issue.description` |
| `executor_id` | `issue.assignee_id`，并设置 `assignee_type=agent` |
| `id` | 生成稳定的外部 origin，作为幂等键 |
| `product_iteration_id` | `issue.metadata.syndra_requirement_id` |
| `project_record_url` | `issue.metadata.project_record_url` |
| `owner_id` | `issue.metadata.syndra_owner_id` |

Issue 初始状态是 `todo`，优先级是 `none`。Multica 会通过 `executor_id` 找到 Agent，再使用该 Agent 的 Multica owner 作为 Issue 创建人，即 `creator_type=member`、`creator_id=<Agent owner ID>`。`owner_id` 是 Syndra 自身的负责人标识，不直接当作 Multica 用户 UUID 使用。

`model` 和 `reasoning_effort` 即使存在也会被忽略，不会覆盖 Multica Agent/runtime 自身配置。`description` 当前也不参与 Issue 正文组装。

以下可选字段会写入 metadata，便于排查：`size`、`development_role`、`state`、`lane_id`、`lane_type`、`executor_kind`、`dispatch_key`、`attempt_id`、`provider_run_id`、`observer_notification_channel`、`observer_notification_id`。其余未知字段允许传入，但当前不参与业务逻辑。

### 标题和正文组装格式

Issue 标题格式：

```text
[SYN:v1:<dispatch_key>] <title>
```

Issue 正文格式：

```text
结构化需求源：
external.product_iteration_id=<product_iteration_id>
project_record_url=<project_record_url>
development_role=<development_role>

<execution_prompt>

---

[SYN:v1:<dispatch_key>]
dispatch_key=<dispatch_key>
lane_id=<lane_id>
work_unit_id=<id>
external.lane_type=<lane_type>
external.provider_run_id=<provider_run_id>
```

`dispatch_key` 按以下顺序确定：显式顶层 `dispatch_key`、`current_attempt.dispatch_key`、以 `syndra-flow-v1:` 开头的 `observer_notification_dispatch_token`；均不存在时，Multica 使用 `syndra-flow-v1:<id>:<attempt_id>` 组装，其中 `attempt_id` 可来自顶层 `attempt_id`、`current_attempt.attempt_id` 或 `current_attempt.id`。如果请求中没有 attempt，则稳定回退为 `syndra-flow-v1:<id>`。

`provider_run_id` 优先使用顶层同名字段，否则读取 `current_attempt.provider_run_id`。`current_attempt` 也兼容直接传入 attempt ID 字符串。上述兼容仅用于组装和排查，不限制 Syndra 在 body 中继续携带其他黑盒字段。

### 请求示例

```json
{
  "id": "wu_<Syndra生成的UUID>",
  "title": "v3.1307_kk协议数据修复",
  "description": "需求的完整描述与验收信息",
  "size": "small",
  "owner_id": "<Syndra负责人ID>",
  "product_iteration_id": 2262,
  "project_record_url": "https://wvyeimw605u.feishu.cn/record/<项目记录ID>",
  "development_role": "fullstack",
  "execution_prompt": "Syndra 生成的完整 qtb-dev-flow-native 执行 Prompt。",
  "model": null,
  "reasoning_effort": null,
  "state": "claimable",
  "lane_id": "lane_<Syndra生成的UUID>",
  "lane_type": "fullstack",
  "executor_kind": "multica",
  "executor_id": "<对应负责人的Multica Agent UUID>",
  "current_attempt": {
    "id": "att_<Syndra生成的UUID>",
    "provider_run_id": "prun_<Syndra生成的UUID>"
  },
  "observer_notification_channel": "multica_requirement_webhook",
  "observer_notification_id": "observer_multica_requirement_webhook_<UUID>",
  "observer_notification_dispatch_token": "observer_dispatch_<UUID>"
}
```

## 联调请求

### 本地联调

```bash
curl --request POST 'https://kiwi-unequal-automated.ngrok-free.dev/api/webhooks/external-issues?sync_type=requirement' \
  -H 'Authorization: Bearer Widp-Pu0BJRfI6-IqU1LYJXg6vRzy5koF9B8W_VdSLac9LXQgIV3YZ0kpJXO9Fcl' \
  -H 'Content-Type: application/json' \
  --data-raw '{
    "id": "wu_<Syndra生成的UUID>",
    "title": "v3.1307_kk协议数据修复",
    "owner_id": "<Syndra负责人ID>",
    "product_iteration_id": 2262,
    "project_record_url": "https://wvyeimw605u.feishu.cn/record/<项目记录ID>",
    "development_role": "fullstack",
    "execution_prompt": "Syndra 生成的完整执行 Prompt",
    "lane_id": "lane_<Syndra生成的UUID>",
    "lane_type": "fullstack",
    "executor_id": "<对应负责人的Multica Agent UUID>",
    "current_attempt": {
      "id": "att_<Syndra生成的UUID>",
      "provider_run_id": "prun_<Syndra生成的UUID>"
    }
  }'
```

### 线上

```bash
curl --request POST 'https://multica-api.lggj.net/api/webhooks/external-issues?sync_type=requirement' \
  -H 'Authorization: Bearer HmLFO9xGsVa0tbSuJMbAaUOArgvNPGFXvDUad4TF3T0euCFY3v_rX7zpJILsCKI6' \
  -H 'Content-Type: application/json' \
  --data-raw '{
    "id": "wu_<Syndra生成的UUID>",
    "title": "v3.1307_kk协议数据修复",
    "owner_id": "<Syndra负责人ID>",
    "product_iteration_id": 2262,
    "project_record_url": "https://wvyeimw605u.feishu.cn/record/<项目记录ID>",
    "development_role": "fullstack",
    "execution_prompt": "Syndra 生成的完整执行 Prompt",
    "lane_id": "lane_<Syndra生成的UUID>",
    "lane_type": "fullstack",
    "executor_id": "<对应负责人的Multica Agent UUID>",
    "current_attempt": {
      "id": "att_<Syndra生成的UUID>",
      "provider_run_id": "prun_<Syndra生成的UUID>"
    }
  }'
```

## 成功响应

首次创建返回 `201 Created`：

```json
{
  "status": "synced",
  "sync_type": "requirement",
  "provider": "syndra",
  "existing": false,
  "source_record_id": "wu_<Syndra生成的UUID>",
  "product_iteration_id": 2262,
  "project_record_url": "https://wvyeimw605u.feishu.cn/record/<项目记录ID>",
  "issue": {
    "id": "<Multica Issue UUID>",
    "title": "[SYN:v1:syndra-flow-v1:wu_<Syndra生成的UUID>:att_<Syndra生成的UUID>] v3.1307_kk协议数据修复",
    "status": "todo",
    "priority": "none",
    "assignee_type": "agent",
    "assignee_id": "<executor_id>"
  }
}
```

相同 `id` 再次推送时返回 `200 OK`，`existing=true`，并重新组装标题、正文和来源 metadata，不重复创建 Issue 或自动开发任务。Issue 在 Multica 内已经变化的状态和优先级会保留。为避免把一个幂等需求静默转交给另一个 Agent，已存在 Issue 的 assignee 与本次 `executor_id` 不一致时返回 `409`。

首次创建前，Multica 会校验 Agent 属于小需求工作区、Agent owner 仍是工作区成员且 Agent runtime 可用；接口确认自动开发任务已成功入队后才返回成功。幂等重推如果已经存在该 Agent 的开发任务，不会再次派发。

## 错误响应

| 状态码 | 场景 | Syndra 建议处理方式 |
|---:|---|---|
| `400` | JSON 非法，缺少必填字段，`executor_id` 不是 UUID，或 `sync_type` 不支持 | 修正请求后使用同一个 `id` 重试 |
| `401` | Bearer Token 缺失或错误 | 校验环境和 Token，不要盲目重试 |
| `409` | 同一 `id` 已存在，但 Issue 当前 assignee 与 `executor_id` 不一致 | 双方确认负责人后再处理，不要自动更换 `id` |
| `422` | `executor_id` 不属于小需求工作区、Agent owner 不是成员，或 Agent/runtime 暂不可执行 | 修正 Agent 配置或等待 runtime 恢复后，使用同一个 `id` 重试 |
| `503` | Multica 未配置 webhook Token 或小需求工作区 | 联系 Multica 服务维护方完成配置 |
| `5xx` | Issue 创建、metadata 更新或自动开发任务入队失败 | 使用指数退避，并使用同一个 `id` 重试 |

Syndra 收到 `200` 或 `201` 后，可以将本次投递视为成功。失败重试必须复用原始 `id`，否则会创建另一条 Issue。

## 日志和安全

- Multica 会记录原始请求体，并在字段校验、幂等查找、Agent/owner/runtime 校验、Issue 创建或刷新、metadata 写入、任务入队等关键节点记录结构化日志。
- 请求体日志可能包含完整 `execution_prompt`。请勿在 body 中放入密码、访问 Token 或私钥。
- `model` 和 `reasoning_effort` 只会记录“字段已收到但被忽略”，不会写入或修改 Agent/runtime。
- 联调时请提供推送时间、`id` 和目标环境，便于 Multica 定位整条链路。
