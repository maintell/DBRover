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

### 1.3.1 单请求内的进度流（`?stream=1`）

分批批量修改要跑几十秒，用「202 + 轮询」要引入任务表与生命周期，代价远大于收益。因此执行类端点在同一个请求里改用 NDJSON：

```
POST /api/v1/changes/{pid}/apply?stream=1
Content-Type: application/x-ndjson

{"type":"progress","done":1000,"total":20000}
{"type":"progress","done":2000,"total":20000}
...
{"type":"result","status":200,"body":{...}}      ← 或 {"type":"error","status":409,"error":{...}}
```

三条硬规则：

1. **状态码只在还没发出第一条 `progress` 时才能改。** 被审批拦下、参数错误都发生在执行开始之前，所以仍然是普通 JSON + 正确状态码；一旦进度开始，HTTP 状态已经固定在 200，错误只能作为最后一行 `error` 出现。
2. **没有进度可报就退回普通 JSON。** 请求了 `stream=1` 但这次执行不分批（影响行数 ≤ 1000、复合主键、无主键，或根本不是批量修改）时，响应仍是 `application/json` 单个对象。客户端按 `Content-Type` 分辨，不用猜。
3. **客户端断开就是中止。** 取消请求（`AbortController` / 关闭连接）会让服务端在批与批之间停下，已提交的批不回滚，返回 `409 CHANGE_APPLY_ABORTED`（连接已断时这条只能落到审计里）。**预案不会被删除**——`SET` 是常量，重新提交是幂等的。

---

## 2. 资源与端点

### 2.0 M1 已实现子集

当前代码只实现了下表中的这一部分。**未列出的端点仍未实现**，不要按本文其余章节假设它们可用。这张表与 `internal/api/server.go` 的路由表由 `TestSpecMatchesRoutes` 双向强制——任一侧多一条，测试就失败。

**请求体字段也受门禁**：服务层结构（`service.DDLRequest` 等）里的每个 json 字段，api 的请求结构都必须能接住——`decode` 用 `DisallowUnknownFields`，接不住的字段会让请求以 `json: unknown field` 被拒。`TestRequestStructsCoverServiceFields` 逐对比较这两侧；刻意不对外暴露的字段要写进该用例的 allowlist 并说明理由（例如 `connection_id`：连接只由 URL 路径决定，否则客户端能把一次写操作指向另一条连接）。

**能力位键名同样受门禁**：界面用 `capabilities.xxx` 决定摆不摆某个入口，键名写错不会报错，那块 UI 会静静消失。`TestUICapabilityKeysExist` 从 `model.Capabilities` 的 json 标签反推，界面引用不存在的键即红。

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/health` | 存活探测，**不需要 token** |
| `GET` | `/api/v1/openapi.json` | 契约文件本身，**不需要 token** |
| `POST` | `/api/v1/connections` | 创建并打开连接 |
| `GET` | `/api/v1/connections` | 连接列表 |
| `DELETE` | `/api/v1/connections/{id}` | 关闭并移除连接 |
| `GET` | `/api/v1/connections/{id}/health` | 版本、只读状态、服务器身份、**能力声明 `capabilities`**（界面据此决定摆不摆「计划」这类入口；不支持就返回 400 `EXPLAIN_UNSUPPORTED`，不等点一次再报错） |
| `GET` | `/api/v1/connections/{id}/tables` | 对象清单：表与视图，引擎能力支持时还包含触发器与序列（每项带 `kind`，界面据此决定能不能点开看数据） |
| `GET` | `/api/v1/connections/{id}/search?q&kind&limit` | 按名称/注释模糊搜索对象（FR-META-04）：`q` 必填；PG 与 MySQL 连表注释、列注释一起搜，SQLite 只搜名字（引擎差异在驱动里，不在契约里分支）。`kind` 逗号分隔做类型过滤（table / view / matview / sequence / trigger / type / routine），**拼错直接 400** 而不是静默忽略——静默忽略会让人以为「这个库里真的没有视图」。`truncated` 表示驱动按 `limit`（默认 50，上限 200）截断过 | ✅ |
| `GET` | `/api/v1/connections/{id}/tables/{name}/rows` | 表数据浏览，**键集分页**（`limit`、`after`、`where`、`order`） |
| `GET` | `/api/v1/connections/{id}/tables/{name}/schema` | 列、索引、外键、主键与建表原文；表不存在 → 404。`?kind=trigger` / `?kind=sequence` 时只回对象定义（`{object, ddl, capabilities}`）——这两类对象没有列结构 |
| `GET` | `/api/v1/connections/{id}/export` | 流式导出 CSV / NDJSON / SQL / Markdown。`format=sql`：建表 + `INSERT` 的可回放脚本（**同引擎**回放）；`format=markdown`：GitHub 风格表格，表头必写、`NULL` 与空串 `''` 分开（与界面一致），`|` 与换行转义；`format=xlsx`：真 Excel 工作簿（`application/vnd.openxmlformats-officedocument.spreadsheetml.sheet`，文件名 `<表>.xlsx`），数字保持数字 |
| `POST` | `/api/v1/connections/{id}/exec` | 执行 SQL（一次性，行数有上限） |
| `POST` | `/api/v1/connections/{id}/query` | 打开**流式游标**，只接受单条只读语句 |
| `GET` | `/api/v1/results/{cid}/page` | 按页拉取，`limit` 可调 |
| `DELETE` | `/api/v1/results/{cid}` | 主动关闭游标 |
| `POST` | `/api/v1/connections/{id}/changes/plan` | 行数据变更预案 |
| `POST` | `/api/v1/connections/{id}/schema/plan` | 结构变更预案（建表 / 加列 / 删列 / 改类型 / 索引建删） |
| `POST` | `/api/v1/connections/{id}/bulk/plan` | 条件批量修改预案 |
| `POST` | `/api/v1/connections/{id}/fk/precheck` | 加外键前的孤儿数据预检（只读） |
| `POST` | `/api/v1/compare` | 两个连接的结构比对（只读） |
| `POST` | `/api/v1/migrate/plan` | 迁移预检（只读）：逐列 `ok`/`review`/`blocked` + 目标端 DDL；`ready=false` 时 `plan` 仍返回，逐列要求人工决定类型。`probe=true` 时额外读源表实际最大字符长度（一次全表扫描，不默认开），逐列回 `actual_max_len`，并据此把「声明收窄」的 `blocked` 翻成 `ok`（实际放得下）或继续阻断并写明实际值 |
| `POST` | `/api/v1/migrate/change` | 把预检的建表语句做成结构预案（不执行）；**不接受客户端 DDL**，语句由服务端重算；执行走 `/changes/{id}/apply` |
| `POST` | `/api/v1/migrate/run` | 搬运一张表的数据并校验（目标表必须已存在）：行数与摘要不一致返回 409 `MIGRATE_VERIFY_FAILED`；HTTP 侧暂不接检查点续跑 |
| `POST` | `/api/v1/changes/{pid}/apply` | 执行上述任一预案；`?stream=1` 用 NDJSON 回报分批进度 |
| `GET` | `/api/v1/audit` | 审计事件 |

与下文契约的已知差异（随里程碑收敛）：

| 差异 | 现状 | 计划 |
|---|---|---|
| `execute` 未独立 | 写操作走 `/exec`；`/exec` 同时兼容一次性只读查询 | 收敛为 `/execute` 只做写、`/query` 只做读 |
| 列元数据随响应返回 | `/query` 直接返回 `columns`，不走 WS 事件 | 改为 WS 推送 `result.columns` |
| 游标按连接隔离在进程内 | 进程重启即失效，无跨实例共享 | 服务化时引入共享存储 |
| 连接支持 SQLite / MySQL / PostgreSQL（Redis 走 KV 那套接口） | 三引擎真机验收通过；MSSQL / Oracle 未实现（镜像拉不下来，见 ADR-0008） | 拿到能拉镜像的机器后按 `docs/04` 补齐 |
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
| `GET` | `/agent/status` | 模型服务是否可用（不返回 key） | ✅ |
| `POST` | `/agent/conversations` | 新建会话（绑定连接 + 模式） | ✅ |
| `GET` | `/agent/conversations/{id}` | 会话详情：元信息（连接、模式、provider、创建/最近使用、`running`）+ 已有记录（`transcript`，与 export 同形状，不含工具结果全文） | ✅ |
| `POST` | `/agent/conversations/{id}/messages` | 发送消息 | ✅ 同步返回；`?stream=1` 改 NDJSON 流（step / answer / done / result / error） |
| `POST` | `/agent/conversations/{id}/approvals/{aid}` | 批准/拒绝（`aid` 为预案 ID） | ✅ |
| `POST` | `/agent/conversations/{id}/abort` | 中止正在跑的一轮：返回 `200 {aborted:true}` 表示**取消已发出**（这一轮在下一个模型调用或工具边界收尾，被中止的 `messages` 请求会带 `stop_message` 返回 200）；没有在跑的一轮时返回 `409 CONVERSATION_IDLE`，不假装停掉了什么 | ✅ |
| `POST` | `/agent/conversations/{id}/compact` | 由人主动压缩一次历史（把最早的工具结果换成指代）：低于预算时 `skipped=true` 且什么都没改；连点幂等 | ✅ |
| `GET` | `/agent/conversations/{id}/export` | 导出（含全部步骤） | ✅ |
| `GET` | `/agent/tools` | 工具清单与 schema（名称、说明、参数 schema、风险与是否写），**不需要连接也不需要模型配置**，按名字排序（顺序稳定才能做快照比对） | ✅ |
| `GET/POST` | `/agent/providers` | Provider 配置 | ⛔（现在走环境变量，见 `docs/11 §8.3`） |

`messages` 的响应形状（未流式时）：

```json
{"answer":"…","steps":[{"index":0,"tool":"query.execute","ok":true,"summary":"3 行 × 1 列",
  "duration_ms":12,"args":{"sql":"SELECT …"}}],
 "notes":[],"usage":{"total_tokens":15},"stopped":"",
 "pending_approvals":[{"plan_id":"…","table":"t","risk":"R3",
   "affected_rows":-1,"statements":[{"sql":"CREATE INDEX …"}],"rollback":[…]}]}
```

`stopped` 非空表示这一轮没走完（`step_budget` / `loop` / `tool_failures` / `duration`），
此时 HTTP 仍是 200：结论与已完成的步骤都要带回去，`stop_message` 说明原因。

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
| `results` | 仅 `/exec` 多语句失败时出现：失败之前已完成的语句结果（含失败那条的 `err`）。错误仍是错误，只是不把已完成的部分丢掉 |
| `category` | `connection` `auth` `permission` `syntax` `constraint` `timeout` `canceled` `resource` `policy` `internal` |
| `message` | 面向人，已脱敏 |
| `hint` | 可执行建议 |
| `engine_detail` | 原生错误码，脱敏后保留 |
| `retryable` | 是否值得重试 |
| `request_id` | 与日志关联 |

### 4.2 错误码表（节选）

| code | category | HTTP | 说明 |
|---|---|:--:|---|
| `DB_CONNECT_FAILED` | connection | 502 | 无法建立连接（**未分类的兜底**：不给修复建议，编一句错的建议比不给更糟） |
| `DB_CONNECT_DNS` | connection | 502 | 域名解析不到（建议：查主机名拼写与 DNS/VPN） |
| `DB_CONNECT_REFUSED` | connection | 502 | 端口拒绝连接 / 拨号失败（服务没起来、端口不对、防火墙或安全组） |
| `DB_CONNECT_TIMEOUT` | connection | 502 | 连接超时（网络不通或被中间设备丢弃） |
| `DB_CONNECT_AUTH` | connection | 502 | **数据库侧**认证失败（用户名/口令/来源限制）。与 API 自己的 `AUTH_TOKEN_INVALID`(401) 是两件事 |
| `DB_CONNECT_NO_DATABASE` | connection | 502 | 库不存在（PG 与 MySQL 连的都是 database） |
| `DB_CONNECT_TLS` | connection | 502 | TLS 握手失败（证书、CA、加密模式与实例要求不一致） |
| `DB_CONNECT_HANDSHAKE` | connection | 502 | TCP 通了但握手被切断（对端不是这个引擎，或被代理/负载均衡掐断） |
| `DB_CONNECT_FILE` | connection | 502 | 本地文件打不开（SQLite 的路径、权限、只读模式） |

> 分类由**驱动**给（它认识引擎错误码），`api` 只把类别翻成上表里的码与建议（FR-CON-03）。
> 三个已实现的引擎都有用例；其中 Windows 的「端口被拒」实测踩到过：内核给的是
> `WSAECONNREFUSED`，与 `syscall.ECONNREFUSED` 不是同一个值，`errors.Is` 匹配不到，
> 所以额外按 `*net.OpError` 的 `Op == "dial"` 兜了一层。
| `DB_NOT_FOUND` | connection | 404 | 库/实例不存在 |
| `DB_PERMISSION_DENIED` | permission | 403 | 权限不足 |
| `SQL_SYNTAX_ERROR` | syntax | 400 | 语法错误（含位置） |
| `SQL_CONSTRAINT_VIOLATION` | constraint | 409 | 唯一/外键/非空冲突 |
| `SQL_TIMEOUT` | timeout | 504 | 语句超时 |
| `SQL_CANCELED` | canceled | 499 | 用户取消 |
| `POLICY_READ_ONLY` | policy | 403 | 只读连接拒绝写 |
| `CROSS_DATABASE_UNSUPPORTED` | syntax | 400 | 语句里有跨库引用（PostgreSQL 的 `库.模式.表`）：一个连接只对应一个库，执行前拦下并说明改法；**不是**策略拦截，补审批也不会过 |
| `POLICY_APPROVAL_REQUIRED` | policy | 428 | 需要审批 |
| `POLICY_APPROVAL_MISMATCH` | policy | 409 | 审批内容与实际语句不一致 |
| `CHANGE_ROW_CONFLICT` | constraint | 409 | 按原值定位不到行，或影响行数与预案不符 |
| `NUMBER_PRECISION_LOSS` | internal | 400 | 大整数被写成 JSON 数字（>2^53 已在解析阶段丢精度），改用字符串重传 |
| `CHANGE_APPLY_ABORTED` | policy | 409 | 分批执行被中止：已提交的批不回滚，预案仍有效 |
| `SCHEMA_CHANGED` | constraint | 409 | 表定义在生成预案后变过 |
| `NARROWING_BLOCKED` | constraint | 409 | 改列类型会截断现存数据 |
| `FK_PRECHECK_BLOCKED` | constraint | 409 | 有孤儿数据或父表列不唯一，加外键会失败（带样本） |
| `DEPENDENCY_BLOCKED` | policy | 409 | 目标仍被视图或入向外键引用，删除会打断它们（提示里列出引用方，近似结果带标记） |
| `LOCK_WAIT_BLOCKED` | connection | 409 | 已有会话在等锁，此时执行拿锁的 DDL 只会让队列更长 |
| `DUPLICATE_ROWS` | constraint | 409 | 唯一索引预检发现重复值 |
| `BULK_CONFIRM_TABLE_REQUIRED` | policy | 428 | 无条件批量修改需回填表名 |
| `BULK_NO_ROLLBACK_ACK` | policy | 428 | 影响行数超过回滚上限，需显式接受 |
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
