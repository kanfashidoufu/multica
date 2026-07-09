# 内部 Bug 平台同步接口文档

本文档用于内部 Bug 管理平台把 Syndra `version_bug` webhook 同步为 Multica issue。

## 接口

`POST https://<multica-api-host>/api/webhooks/external-issues?sync_type=bug`

必须携带 `sync_type=bug`。没有该参数时，请求会继续走现有飞书需求导入逻辑。

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
| `MULTICA_EXTERNAL_ISSUE_DEFAULT_WORKSPACE_ID` | 默认创建 issue 的 workspace UUID |
| `MULTICA_EXTERNAL_ISSUE_DEFAULT_LARK_USER_ID` | 默认同步负责人，对应已绑定 Multica 成员的飞书 external user id/open id/union id |

也可以用 query 覆盖：

| Query | 说明 |
|---|---|
| `workspace_id` | 本次同步目标 workspace UUID |
| `assignee_user_id` | 本次同步使用的默认负责人外部用户 ID |

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
| `items[].status/status_name` | 映射为 Multica issue 状态 |
| `items[].bug_level/priority` | 映射为 Multica issue 优先级 |
| `items[].bug_type_id/bug_type` | 写入 issue metadata |
| `items[].creator/owner/assignee/solver` | 写入 issue metadata；创建 issue 时只用 `assignee.name` 精确匹配 Multica 用户名，匹配不到或不唯一时使用默认指派人，不使用 `mate_id` |
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
| `503` | Multica 未配置 webhook token 或外部导入未配置 |
| `400` | JSON 请求体无效、workspace/record/title 缺失 |
| `422` | 默认负责人未配置，或该负责人不是目标 workspace 成员 |
