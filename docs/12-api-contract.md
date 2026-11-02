# 12 · 接口契约

> OpenAPI 3.1 是唯一真相（`api/openapi.yaml`），Go server 类型与 TS client 由代码生成。本文说明**设计约定**，字段级定义以 spec 为准。

---

## 1. 通用约定

### 1.1 基础

| 项 | 约定 |
|---|---|
| Base URL | `http://127.0.0.1:<port>/api/v1` |
| 鉴权 | 请求头 `X-DBRover-Token: <token>`（每进程随机生成） |
| 内容类型 | `application/json; charset=utf-8`（结果分片除外） |
| ID | ULID 字符串 |
| 时间 | RFC3339 UTC（`2026-11-02T10:12:03Z`） |
| 命名 | 路径 `kebab-case`，字段 `snake_case` |
| 幂等 | 写操作支持 `Idempotency-Key` 头（可选，24h 内去重） |

### 1.2 分页

**列表类**用游标分页：

```
GET /objects?cursor=<opaque>&limit=100
→ {"items":[...], "next_cursor":"...", "has_more":true}
```

**结果集**用独立游标（见 §3），不用上面的通用分页——语义不同（一个是元数据列表，一个是活的结果集）。

### 1.3 长任务

创建即返回任务 id，进度通过 WS 或轮询：

```
POST /tasks/backup  → 202 {"task_id":"..."}
GET  /tasks/{id}    → {"status":"running","progress":{...}}
```

`202 Accepted` 表示已受理，不表示完成。

---

## 2. 资源与端点

### 2.0 M1 已实现子集

当前代码只实现了下表中的一小部分。**未列出的端点仍未实现**，不要按本文其余章节假设它们可用。

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/health` | 存活探测，**不需要 token** |
| `GET` | `/api/v1/openapi.json` | 契约文件本身，**不需要 token** |
| `POST` | `/api/v1/connections` | 创建并打开连接 |
| `GET` | `/api/v1/connections` | 连接列表 |
| `DELETE` | `/api/v1/connections/{id}` | 关闭并移除连接 |
| `GET` | `/api/v1/connections/{id}/health` | 版本、只读状态、服务器身份 |
| `GET` | `/api/v1/connections/{id}/tables` | 表与视图清单 |
| `GET` | `/api/v1/connections/{id}/tables/{name}/rows` | 表数据浏览，**键集分页**（`limit`、`after`） |
| `POST` | `/api/v1/connections/{id}/exec` | 执行 SQL（一次性，行数有上限） |
| `POST` | `/api/v1/connections/{id}/query` | 打开**流式游标**，只接受单条只读语句 |
| `GET` | `/api/v1/results/{cid}/page` | 按页拉取，`limit` 可调 |
| `DELETE` | `/api/v1/results/{cid}` | 主动关闭游标 |
| `GET` | `/api/v1/audit` | 审计事件 |

与下文契约的已知差异（随里程碑收敛）：

| 差异 | 现状 | 计划 |
|---|---|---|
| `execute` 未独立 | 写操作走 `/exec`；`/exec` 同时兼容一次性只读查询 | 收敛为 `/execute` 只做写、`/query` 只做读 |
| 列元数据随响应返回 | `/query` 直接返回 `columns`，不走 WS 事件 | 改为 WS 推送 `result.columns` |
| 游标按连接隔离在进程内 | 进程重启即失效，无跨实例共享 | 服务化时引入共享存储 |
| 连接只支持 SQLite | 其他引擎返回 `DB_CONNECT_FAILED` | 按 04 文档逐个补齐 |
| 契约已落文件但未接代码生成 | 契约在 `internal/api/openapi.json`，由测试保证与路由表一致 | 接 `oapi-codegen` 生成 server 类型与 TS client |

**契约的可信度靠门禁保证，不靠自觉**：

- `TestSpecMatchesRoutes` 双向比对路由表与 `paths`，任一侧多一条即失败；
- `TestSpecSecurityMatchesPublic` 要求免鉴权端点显式写 `"security": []`，需鉴权端点不得覆盖全局声明；
- `TestSpecIsWellFormed` 检查每个操作都有 `summary` 与 `responses`。

门禁本身经过变异验证：临时加一条路由会让 `TestSpecMatchesRoutes` 失败。

**键集分页的边界**（`.../tables/{name}/rows`）：只对**有主键的表**生效。无主键表返回 `501 CAPABILITY_UNSUPPORTED`，因为缺少稳定定位键时任何分页都会漏行或重复。`after` 是 JSON 字符串数组（如 `["42"]`），由服务端按列类型还原为非字符串值——用字符串传值是为了避免 JSON 数字在大整数主键上丢精度。

**已实现的两条契约细节**

- **执行期错误走错误模型**：驱动错误经 `service.ClassifyError` 映射为 `code`/`category`/`hint`，HTTP 状态按 §4.2 表返回（语法 400、约束 409、超时 504、取消 499）。不再出现「200 + `results[].err`」。
- **截断必须显式**：`GET /results/{cid}/page` 返回 `truncated`。到达 `max_rows` 与「读尽」是两件事，前者不能让客户端误判为全量。

---

### 2.1 连接

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/connections` | 列表（不含凭据） |
| `POST` | `/connections` | 创建 |
| `GET` | `/connections/{id}` | 详情 |
| `PATCH` | `/connections/{id}` | 更新（部分字段） |
| `DELETE` | `/connections/{id}` | 删除（同时清理凭据） |
| `POST` | `/connections/{id}/test` | 连接测试（返回可诊断错误） |
| `POST` | `/connections/{id}/open` | 建立池 |
| `POST` | `/connections/{id}/close` | 关闭池 |
| `GET` | `/connections/{id}/health` | 版本、时区、只读状态、权限摘要 |
| `GET` | `/connections/export` | 导出（凭据脱敏） |
| `POST` | `/connections/import` | 导入 |

**创建请求示例**

```json
POST /connections
{
  "name": "shop-prod",
  "engine": "mysql",
  "environment": "production",
  "host": "10.0.0.12",
  "port": 3306,
  "catalog": "shop",
  "params": {"charset": "utf8mb4", "time_zone": "+08:00"},
  "access_mode": "read-only",
  "tunnel": {
    "hops": [{
      "addr": "bastion.example.com:22",
      "user": "ops",
      "auth_kind": "private_key",
      "credential": {"kind": "private_key", "value": "<inline-once>"}
    }]
  },
  "credential": {"kind": "password", "value": "<inline-once>"}
}
```

**环境字段的写入规则**

- 省略 `environment` → 服务端置为 `unknown`（严格度等同 `production`）。
- **放宽**（新的严格度低于当前值）必须同时传 `"acknowledge_loosening": true`，否则返回 `POLICY_LOOSENING_NOT_ACKNOWLEDGED`（409），并在审计中记录原值与新值。
- **收紧**（更高严格度）永远允许，无需确认。
- `environment` 不可通过批量导入静默下发：导入时一律置为 `unknown`，由用户逐个确认（团队模式下由管理员下发除外）。

**凭据写入规则**：`value` 只在**创建/更新请求**中出现一次，服务端立即存入 OS 凭据库，响应与后续查询只返回 `credential_ref`。响应中永不回显。

### 2.2 元数据

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/connections/{id}/catalogs` | 库列表 |
| `GET` | `/connections/{id}/catalogs/{cat}/schemas` | 模式列表 |
| `GET` | `/connections/{id}/objects` | 对象列表（`schema`、`kind`、`cursor`、`limit`） |
| `GET` | `/connections/{id}/objects/{kind}/{name}` | 对象详情（`schema` 作为 query） |
| `GET` | `/connections/{id}/objects/ddl` | DDL 文本（`schema`、`kind`、`name`） |
| `GET` | `/connections/{id}/search` | 对象搜索（`q`、`kinds`、`limit`） |
| `GET` | `/connections/{id}/profile` | 库画像（P1） |
| `POST` | `/connections/{id}/refresh-metadata` | 强制刷新缓存 |

### 2.3 查询

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/connections/{id}/query` | 执行（返回列元数据 + 游标） |
| `POST` | `/connections/{id}/execute` | 执行（不返回行，返回影响行数） |
| `GET` | `/results/{cursor_id}/page` | 取一页 |
| `DELETE` | `/results/{cursor_id}` | 关闭游标 |
| `POST` | `/sessions/{session_id}/cancel` | 取消在途语句 |
| `POST` | `/connections/{id}/explain` | 执行计划 |

**`POST /query` 请求**

```json
{
  "session_id": "01JC...",
  "sql": "SELECT id, status FROM \"order\" WHERE id > $1 ORDER BY id LIMIT 200",
  "args": [1000],
  "page_size": 200,
  "keyset": {"order_by": ["id"]},
  "stop_on_error": true,
  "max_rows": 100000
}
```

**响应（不含行，行通过游标取）**

```json
{
  "execution_id": "01JC...",
  "statements": [{
    "index": 0,
    "risk": "R0",
    "cursor_id": "01JC...",
    "columns": [
      {"name":"id","type":{"kind":"integer","bits":64},"nullable":false,"source":{"table":"order","column":"id"}},
      {"name":"status","type":{"kind":"string","max_len":16},"nullable":true}
    ],
    "pagination": {"kind":"keyset","order_by":["id"]},
    "total_estimate": null
  }],
  "duration_ms": 42
}
```

**`GET /results/{id}/page` 响应**用 `application/octet-stream` 的紧凑格式（列式分块），调试模式可加 `?format=json`。

### 2.4 变更

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/changes/plan` | 生成预案（DDL/DML） |
| `GET` | `/changes/{id}` | 预案详情 |
| `POST` | `/changes/{id}/approve` | 审批（生成 approval 记录） |
| `POST` | `/changes/{id}/apply` | 执行（需 `approval_id`） |
| `POST` | `/changes/{id}/rollback` | 回滚（生成反向预案并执行） |
| `DELETE` | `/changes/{id}` | 丢弃草稿 |

**`POST /changes/plan` 请求**

```json
{
  "connection_id": "01JC...",
  "kind": "ddl",
  "targets": [{"schema":"public","kind":"table","name":"order"}],
  "intent": {
    "op": "add_column",
    "column": {"name":"channel_code","type":{"kind":"string","max_len":32},"nullable":true}
  }
}
```

也支持 `"sql": "..."` 直接提交语句（用于手写 DDL 的预案化）。

**响应**

```json
{
  "id": "01JC...",
  "risk": "R3",
  "statements": ["ALTER TABLE \"public\".\"order\" ADD COLUMN \"channel_code\" varchar(32)"],
  "rollback": ["ALTER TABLE \"public\".\"order\" DROP COLUMN \"channel_code\""],
  "rollback_kind": "compensating",
  "impact": {
    "rows_affected": 120345,
    "tables_touched": ["public.order"],
    "lock": "ACCESS EXCLUSIVE",
    "est_duration": "秒级",
    "online_ddl_hint": null,
    "dependencies": []
  },
  "preconditions": [{"check":"no_long_transaction","ok":true}],
  "statement_hash": "sha256:..."
}
```

`statement_hash` 是审批与执行绑定的一致性凭证。

### 2.5 监控

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/connections/{id}/monitor/summary` | 首屏四卡 + 结论 |
| `GET` | `/connections/{id}/monitor/sessions` | 会话列表 |
| `POST` | `/connections/{id}/monitor/sessions/{sid}/cancel` | 取消语句 |
| `POST` | `/connections/{id}/monitor/sessions/{sid}/kill` | 终止会话（需确认） |
| `GET` | `/connections/{id}/monitor/slow-queries` | 慢查询（`window=15m`） |
| `GET` | `/connections/{id}/monitor/locks` | 阻塞链 |
| `GET` | `/connections/{id}/monitor/metrics` | 指标时序 |
| `GET` | `/connections/{id}/monitor/capacity` | 体量 |

### 2.6 任务（备份/迁移）

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/tasks/backup` | 创建备份任务 |
| `POST` | `/tasks/restore` | 创建恢复任务 |
| `POST` | `/tasks/migration` | 创建迁移任务 |
| `GET` | `/tasks` | 列表 |
| `GET` | `/tasks/{id}` | 详情与进度 |
| `POST` | `/tasks/{id}/pause` | 暂停 |
| `POST` | `/tasks/{id}/resume` | 续跑 |
| `POST` | `/tasks/{id}/cancel` | 取消 |
| `GET` | `/backups/inspect` | 浏览备份产物（`path`） |

### 2.7 Agent

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/agent/conversations` | 新建会话（绑定连接 + 模式） |
| `GET` | `/agent/conversations/{id}` | 会话详情 |
| `POST` | `/agent/conversations/{id}/messages` | 发送消息（SSE 或 WS 流式返回） |
| `POST` | `/agent/conversations/{id}/approvals/{aid}` | 批准/拒绝 |
| `POST` | `/agent/conversations/{id}/abort` | 中止 |
| `GET` | `/agent/conversations/{id}/export` | 导出（含全部步骤） |
| `GET` | `/agent/tools` | 工具清单与 schema |
| `GET/POST` | `/agent/providers` | Provider 配置 |

### 2.8 审计与设置

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/audit` | 查询（`from`、`to`、`actor`、`action`、`target`、`cursor`） |
| `GET` | `/audit/verify` | 校验链完整性 |
| `GET` | `/audit/export` | 导出 JSONL |
| `GET/PATCH` | `/settings` | 全局设置 |
| `GET` | `/history` | 查询历史 |
| `GET/POST` | `/favorites` | 收藏 |
| `GET/PUT` | `/workspace` | 工作区布局 |

---

## 3. 结果游标语义

| 项 | 规则 |
|---|---|
| 生命周期 | 创建后 5 分钟无访问即过期；每次拉取续期 |
| 并发 | 同一游标只允许一个读取请求（否则返回 `CURSOR_BUSY`） |
| 关闭 | `DELETE` 或超时；关闭后拉取返回 `CURSOR_CLOSED` |
| 顺序 | 严格按查询的 `ORDER BY`；无 `ORDER BY` 时顺序不保证，且**不可用 keyset** |
| 最大行数 | 达到 `max_rows` 后返回 `exhausted: false, truncated: true` |

**为什么服务端持有游标**：避免每次翻页重跑查询（深分页会越来越慢），也避免客户端缓存全量。

---

## 4. 错误模型

### 4.1 统一结构

```json
{
  "error": {
    "code": "DB_AUTH_FAILED",
    "category": "auth",
    "message": "Access denied for user 'ro'@'10.0.0.5'",
    "hint": "请检查用户名与密码；该实例可能要求指定 authSource",
    "engine_detail": {"errno": 1045, "sqlstate": "28000"},
    "retryable": false,
    "request_id": "01JC..."
  }
}
```

| 字段 | 用途 |
|---|---|
| `code` | 稳定枚举，前端/Agent 据此分支（**不要匹配 message**） |
| `category` | `connection` `auth` `permission` `syntax` `constraint` `timeout` `canceled` `resource` `policy` `internal` |
| `message` | 面向人，已脱敏 |
| `hint` | 可执行建议 |
| `engine_detail` | 原生错误码，脱敏后保留 |
| `retryable` | 是否值得重试 |
| `request_id` | 与日志关联 |

### 4.2 错误码表（节选）

| code | category | HTTP | 说明 |
|---|---|:--:|---|
| `DB_CONNECT_FAILED` | connection | 502 | 无法建立连接 |
| `DB_DNS_FAILED` | connection | 502 | 主机解析失败 |
| `DB_TLS_FAILED` | connection | 502 | TLS 握手失败 |
| `DB_AUTH_FAILED` | auth | 401 | 认证失败 |
| `DB_NOT_FOUND` | connection | 404 | 库/实例不存在 |
| `DB_PERMISSION_DENIED` | permission | 403 | 权限不足 |
| `SQL_SYNTAX_ERROR` | syntax | 400 | 语法错误（含位置） |
| `SQL_CONSTRAINT_VIOLATION` | constraint | 409 | 唯一/外键/非空冲突 |
| `SQL_TIMEOUT` | timeout | 504 | 语句超时 |
| `SQL_CANCELED` | canceled | 499 | 用户取消 |
| `POLICY_READ_ONLY` | policy | 403 | 只读连接拒绝写 |
| `POLICY_APPROVAL_REQUIRED` | policy | 428 | 需要审批 |
| `POLICY_APPROVAL_MISMATCH` | policy | 409 | 审批内容与实际语句不一致 |
| `CURSOR_CLOSED` | resource | 410 | 游标已关闭 |
| `CURSOR_BUSY` | resource | 409 | 游标正在被读取 |
| `TASK_CONFLICT` | resource | 409 | 任务状态不允许该操作 |
| `CAPABILITY_UNSUPPORTED` | internal | 501 | 该引擎不支持此能力 |
| `INTERNAL` | internal | 500 | 未分类内部错误 |

**`CAPABILITY_UNSUPPORTED` 必须带 `hint` 说明替代方案**，例如「该引擎不支持 keyset 分页，已降级为偏移分页」。

---

## 5. WebSocket 事件

连接后先 `subscribe`，事件推送用于长时间操作与后台状态。

```json
{"type":"subscribe","topics":["execution:*","task:*","agent:*"]}
```

| 事件 | 载荷 |
|---|---|
| `execution.started` | `{execution_id, statement_count}` |
| `execution.statement_done` | `{execution_id, index, duration_ms, rows}` |
| `execution.finished` | `{execution_id, status, duration_ms}` |
| `result.columns` | `{cursor_id, columns}` |
| `task.progress` | `{task_id, done, total, current_item, eta_s}` |
| `task.finished` | `{task_id, status, report_url}` |
| `agent.step` | `{conversation_id, seq, kind, tool}` |
| `agent.token` | `{conversation_id, delta}`（流式文本） |
| `agent.approval_required` | `{conversation_id, approval_id, risk, sql, impact}` |
| `monitor.alert` | `{connection_id, level, title, detail}` |
| `connection.state` | `{connection_id, state}`（`connected`/`reconnecting`/`failed`） |

**背压**：服务端发送队列有界（默认 1024 条）；溢出时发送 `{"type":"overflow"}` 并暂停该 topic，客户端需重连并做全量刷新。

---

## 6. 版本与兼容

- 路径带主版本 `/api/v1`。
- 新增字段向后兼容；删除/改语义需升版本。
- OpenAPI 快照存于仓库，CI 比对：出现破坏性变更（删除字段、改类型、收紧校验）必须附 ADR 批准。
- 前端与核心同仓同发，但仍保持契约稳定——这是「服务化」与「外部集成」的前提。

---

## 7. 安全

| 项 | 规则 |
|---|---|
| 鉴权 | 所有端点需 token；`GET /health`（无版本前缀）除外 |
| Origin | 校验 `Origin` 为 `null`（桌面）或白名单 |
| CSRF | 不使用 Cookie，无 CSRF 面 |
| 速率 | 默认不限；`agent/*` 与 `search` 有限流 |
| 日志 | 请求日志不含请求体中的 `credential.value` |
| 错误 | 不返回堆栈；`request_id` 用于关联 |
