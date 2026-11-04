# 03 · 领域模型

> 本文定义 DBRover 内部的实体、关系、状态机与持久化结构。所有层共用 `internal/model`，不得各自造模型。

---

## 1. 实体总览

```
Connection ──┬── Credential(handle)          # 凭据句柄，非密码
             ├── TunnelConfig ── Hop[]       # SSH 跳板链
             ├── AccessMode                  # read-only | read-write | admin
             └── Session[]                   # 运行时
                    ├── Transaction?
                    └── Statement[]

ChangePlan ── Statement[] ── Approval? ── Apply(Execution)
Task       ── Step[]                        # backup | migration | index build
Execution  ── AuditEvent[]
AgentConversation ── AgentStep[] ── ToolCall ── Approval?

MetadataSnapshot ── Catalog[] ── Schema[] ── Object[]
```

---

## 2. 核心实体

### 2.1 Connection

一个可复用的连接配置。**不持有物理连接**，物理连接由 `service/connection` 的池管理。

> **v1 实现口径**：实际结构见 `internal/model/model.go` 的 `Connection`——只有 `id` / `name` / `engine` / `host` / `port` / `catalog` / `schema` / `environment` / `environment_source` / `access_mode` / `display_timezone` / `server_identity`。下表其余字段（`params` / `tunnel` / `credential` / `environment_declared_*` / `pool` / `init_sql` / `color` `tags` / `created_at` `updated_at`）**尚未实现**，属目标模型；连接目前只存在进程内存里，没有持久化的元数据库。

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string(ULID) | 稳定标识 |
| `name` | string | 显示名，工作区内唯一 |
| `engine` | enum | `postgres` `mysql` `mariadb` `mssql` `oracle` `sqlite` `redis` `mongodb` `elasticsearch` |
| `host` `port` | string / int | `sqlite` 时为文件路径 |
| `catalog` `schema` | string? | 默认打开的上下文 |
| `params` | map[string]string | 引擎特有：`sslmode`、`charset`、`service_name`、`auth_source`、`replica_set` 等 |
| `tunnel` | TunnelConfig? | 见 2.3 |
| `credential` | CredentialRef | 见 2.2 |
| `environment` | enum | `local` / `test` / `staging` / `production` / `unknown`。**新连接默认 `unknown`，策略等同生产** |
| `environment_source` | enum | `declared`（人工声明）/ `detected`（自动推断）/ `inherited`（团队下发） |
| `environment_declared_at` `environment_declared_by` | time / string | 放宽到更低严格度时必填，用于审计 |
| `server_identity` | ServerIdentity | 连接后采集的真实服务器指纹（主机名 + 实例标识），用于防误连；不持久化敏感信息 |
| `access_mode` | enum | 创建时默认**随环境**：生产/预发 `read-only`，测试/本地 `read-write` |
| `pool` | PoolConfig | 最大连接数、空闲回收、超时 |
| `init_sql` | []string | 连接建立后执行的会话级语句（如 `SET search_path`） |
| `color` / `tags` | — | 视觉分组 |
| `created_at` `updated_at` | time | — |

**不变量**

- `access_mode = read-only` 时，`Guard` 必须在协议层拒绝对话级写（见 10 号文档）。
- `credential` 只存句柄；序列化到磁盘时不含任何密文以外的内容（密文本身也不落盘）。
- `sqlite` 引擎的 `tunnel` 必须为空（本地文件）。
- `environment` 只能由人放宽（`local`/`test`），自动检测只能收紧。
- `environment_source = detected` 时，其严格度**不得低于**当前声明值（检测只能抬高）。

### 2.2 Credential

| 字段 | 说明 |
|---|---|
| `id` | 句柄，形如 `keyring://dbrover/<connection_id>` |
| `kind` | `password` `private_key` `key_passphrase` `token` `client_cert` |
| `backend` | `wincred` `keychain` `secret_service` `file(0600)` `none` |
| `label` | 供用户识别（如「生产只读账号」） |

**规则**

1. 密码、私钥内容、口令一律只进 OS 凭据库。
2. 内存中持有的明文以 `[]byte` 存放，使用后零化，且不进入任何日志/DTO。
3. 导出连接配置时，凭据字段替换为 `{"__redacted": true}`。
4. Linux 若 Secret Service 不可用，降级为 `file(0600)` 并在 UI 明确警告「凭据以受保护文件存储，安全性低于系统密钥环」。

### 2.3 TunnelConfig / Hop

```go
type TunnelConfig struct {
    Hops []Hop   // 顺序串联，最后一个 Hop 的 host 才是数据库
}

type Hop struct {
    Addr     string        // host:port
    User     string
    AuthKind AuthKind      // password | private_key | agent
    Credential CredentialRef
    HostKey  string        // known_hosts 指纹（TOFU 后固定）
}
```

**规则**

- 首次连接采用 TOFU（首次使用即信任）并记录指纹；指纹变化必须阻断并提示中间人风险。
- 跳板链长度上限 4，超过提示改用直连或堡垒机。
- 隧道端口在 `127.0.0.1` 随机分配，连接关闭即释放。

### 2.4 Session

逻辑会话，绑定到一个 `Connection`。

| 字段 | 说明 |
|---|---|
| `id` | ULID |
| `connection_id` | — |
| `catalog` `schema` | 当前上下文，可切换 |
| `state` | `idle` `running` `in_tx` `broken` `closed` |
| `pinned_conn` | `in_tx` 时钉住的物理连接标识 |
| `tx` | `Transaction?` |

**状态机**

```
        ┌──────────── open ─────────────┐
        ▼                               │
    (idle) ──Execute──► (running) ──ok──┘
        │                  │
        │                  └──BEGIN──► (in_tx) ──COMMIT/ROLLBACK──► (idle)
        │                                  │
        └──conn lost / fatal error──► (broken) ──reconnect──► (idle)
                                           │
                                      close ──► (closed)
```

**关键约束**

- `in_tx` 状态下不接受并行语句（引擎本身也不允许，提前拦截给出清晰提示）。
- `broken` 后未提交事务视为已回滚，UI 必须显式告知。
- Session 关闭时若有未提交事务 → 回滚并记审计。

### 2.5 Execution / Statement

**Execution** 是一次用户或 Agent 发起的执行批次（审计单位）；**Statement** 是其中一条语句（诊断单位）。

| Execution 字段 | 说明 |
|---|---|
| `id` | ULID，贯穿日志与审计（`execution_id`） |
| `initiator` | `human:<user>` / `agent:<conversation_id>` / `schedule:<task_id>` |
| `connection_id` `session_id` | — |
| `sql_text` | 原始文本（截断到 64KB，超出存哈希 + 前缀） |
| `risk` | `R0..R4` |
| `started_at` `duration_ms` | — |
| `rows_affected` `rows_returned` | — |
| `status` | `ok` `error` `canceled` `timeout` |
| `error` | 结构化错误（脱敏） |
| `approval_id` | 若经过审批 |

| Statement 字段 | 说明 |
|---|---|
| `index` | 在批次中的序号 |
| `sql_text` `normalized` | 归一化文本（字面量替换为 `?`，用于聚合） |
| `plan_digest` | 执行计划摘要 hash（可选采集） |
| `duration_ms` `rows` | — |
| `engine_meta` | 引擎特有：影响行数/警告数/最后插入 ID 等 |

### 2.6 ResultCursor

分页结果的句柄。

| 字段 | 说明 |
|---|---|
| `id` | ULID |
| `columns` | 名称、类型（中立）、可空、来源表与列 |
| `pagination` | `keyset{order_by, last_key}` 或 `offset{page_size, offset}` |
| `total_estimate` | 可选（仅当引擎能廉价给出，如 `COUNT(*) OVER()` 或索引统计） |
| `state` | `open` `exhausted` `closed` `expired` |
| `lifetime` | 默认 5 分钟，拉取时续期 |

**决策：不缓存全量结果**。理由：100 万行结果集全量缓存会 OOM；且用户真正需要的通常是前几百行。导出走独立的流式通道。

### 2.7 ChangePlan

变更预案，是「安全变更」的载体。

| 字段 | 说明 |
|---|---|
| `id` | ULID |
| `kind` | `ddl` `dml` `migration` |
| `target` | connection + 对象引用列表 |
| `statements` | 正向语句（有序） |
| `rollback` | 回滚语句（有序，可能为空 → 则标记 `irreversible`） |
| `impact` | `{rows_estimated, tables_touched, locks, est_duration, online_ddl_hint}` |
| `risk` | 取所有语句的最高级别 |
| `preconditions` | 执行前检查（如表仍存在、无长事务） |
| `status` | `draft` `approved` `applied` `failed` `rolled_back` `discarded` |

> **v1 实现口径**：实际结构见 `internal/service/change.go` 的 `ChangePlan`——`id` / `table` / `statements` / `rollback` / `risk` / `friction` / `irreversible` / `created_at` / `expires_at` / `undo` / `ddl` / `rolls_back_as_compensation` / `affected_rows` / `notes`，外加两个只在服务端保管的竞态前置条件（`expect_ddl_hash`、`expect_table_absent`）。**`impact` / `preconditions` / `status` 与下面的状态机尚未实现**：v1 用「预案只在服务端保管 + 10 分钟有效期 + 表定义哈希 + 执行前预检」表达同一件事，没有 draft/approved 这种显式状态字段。

**状态机（目标）**

```
draft ──approve──► approved ──apply──► applied
  │                   │                  │
  │                   └──apply fails──► failed ──retry──► approved
  │                                      │
  └──discard──► discarded          rollback──► rolled_back
```

**不变量**

- `risk >= R3` 且 `rollback` 为空 → 必须标记 `irreversible`，UI 要求输入对象名确认。
- `applied` 后不可修改，只能 `rollback`（生成新的反向 plan）。
- `approved` 有有效期（默认 10 分钟），过期需重新审批。

### 2.8 Task（备份 / 迁移 / 索引构建）

| 字段 | 说明 |
|---|---|
| `id` | ULID |
| `kind` | `backup` `restore` `migration` `index_build` `runbook` |
| `spec` | 类型相关的参数（源、目标、对象清单、选项） |
| `progress` | `{total_units, done_units, current_item, bytes_written, eta}` |
| `checkpoint` | 可续跑的位置（表名 + 主键区间 / 文件偏移） |
| `status` | `queued` `running` `paused` `succeeded` `failed` `canceled` |

**状态机**

```
queued ─► running ─┬─► succeeded
                   ├─► failed ──resume──► running
                   ├─► paused ──resume──► running
                   └─► canceled
```

**关键**：`checkpoint` 是幂等的前提。迁移按「表 + 主键区间」推进，每批完成即落 checkpoint + 幂等键（批内主键集合的哈希），重启时跳过已提交批次。**不使用自增偏移量**（源表并发写入会错位）。

### 2.9 AgentConversation / AgentStep

| AgentConversation | 说明 |
|---|---|
| `id` | ULID |
| `connection_id` | 绑定的连接（决定权限天花板） |
| `permission_mode` | `read-only` `write-with-approval` `autonomous` |
| `provider` `model` | 使用的模型 |
| `mode` | `chat` `plan` `runbook` |
| `token_usage` | 累计 |
| `status` | `active` `completed` `aborted` |

| AgentStep | 说明 |
|---|---|
| `seq` | 序号 |
| `kind` | `think` `tool_call` `tool_result` `message` `approval_request` `error` |
| `tool` | 工具名（与 service 方法同名，如 `query.execute`） |
| `args` `result_digest` | 参数与其结果摘要（完整结果另存，截断） |
| `duration_ms` | — |
| `cost_tokens` | — |

**规则**：`AgentStep` 必须能**完整重放**一次会话（用摘要 + 参数重跑工具）。这是排障与信任的基础。

### 2.10 AuditEvent

不可变追加日志。

| 字段 | 说明 |
|---|---|
| `id` | ULID（单调递增，便于游标） |
| `at` | 时间 |
| `actor` | `human` / `agent` / `schedule` / `system` |
| `actor_id` | 用户/会话/任务 id |
| `action` | `query.execute` `change.apply` `conn.create` `session.kill` `credential.read` … |
| `target` | 连接 id / 对象引用 |
| `risk` | 可选 |
| `outcome` | `ok` `denied` `error` |
| `detail` | 结构化，**默认不含结果数据** |
| `prev_hash` `hash` | 链式哈希，防篡改 |

**约束**：只追加，不更新不删除；提供按时间/actor/target 查询与导出。

---

## 3. 元数据模型

### 3.1 统一对象树

```go
type Server struct {
    Engine   Engine
    Version  string
    Catalogs []Catalog
}

type Catalog struct {
    Name    string
    Schemas []Schema
    // 引擎差异：MySQL 的 Catalog 无 Schema；Redis 的 Catalog 是 db index
}

type Schema struct {
    Name    string
    Objects []ObjectRef
}

type ObjectRef struct {
    Kind ObjectKind   // table|view|matview|routine|sequence|type|trigger|index|collection
    Name string
    // 惰性：只有被展开时才加载详情
}

type TableDetail struct {
    Columns     []Column
    Indexes     []Index
    ForeignKeys []ForeignKey
    Checks      []Check
    Partitions  []Partition
    Comment     string
    RowEstimate int64
    SizeBytes   int64
    EngineExtra map[string]any   // 引擎特有，UI 按引擎渲染
}

type Column struct {
    Name        string
    Type        ColumnType   // 中立类型 + 引擎原始类型串
    Nullable    bool
    Default     *string
    Comment     string
    IsPrimary   bool
    IsUnique    bool
    Ordinal     int
    IsGenerated bool
}
```

**中立类型系统**（用于跨引擎迁移与 UI 渲染）：

```
Integer{bits}  Decimal{precision,scale}  Float{bits}
String{max_len, fixed}  Text            Binary{max_len}
Boolean  Date  Time  Timestamp{with_tz}  Interval
UUID  JSON  Array{elem}  Enum{values}  Set{values}
Geometry{kind}  Network{kind}  Vector{dim}
Domain{base}  Composite{fields}  Range{subtype}
```

每个 `ColumnType` 同时保留 `Raw`（引擎原始类型字符串），因为「精确还原」比「统一」更重要——迁移时优先用 `Raw` 的映射而不是中立类型反推。

### 3.2 能力描述

```go
type Capabilities struct {
    Transactions      bool
    Schemas           bool
    Views             bool
    MaterializedViews bool
    Routines          bool
    Triggers          bool
    Sequences         bool
    UserTypes         bool
    Indexes           bool
    Explain           bool
    SlowQueryStats    bool
    SessionMgmt       bool
    KillSession       bool
    LockGraph         bool
    Backup            bool
    BulkLoad          bool
    KeysetPagination  bool
    DDLTransactional  bool     // PG/SQLite: true; MySQL: false（DDL 隐式提交）
}
```

UI 依据它渲染或置灰；`service` 依据它拒绝非法调用并给出可解释错误。**不允许用 `switch engine` 代替 `Capabilities()`。**

> **v1 实现口径**：字段类型统一是 `bool`（没有 `IndexCapability` / `ExplainCapability` / `BackupCapability` 这些复合类型，也没有 `ReadOnlyEnforce`）。只读不是能力位，而是连接级 `AccessMode` 加驱动层强制（SQLite `PRAGMA query_only`、MySQL / PG 的会话只读），所以不需要那个策略枚举。

---

## 4. 持久化结构（SQLite）

```sql
-- 元数据、历史、审计、会话记录
CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);

CREATE TABLE connection (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, engine TEXT NOT NULL,
  host TEXT, port INTEGER, catalog TEXT, schema_name TEXT,
  params_json TEXT NOT NULL DEFAULT '{}',
  tunnel_json TEXT,
  credential_ref TEXT,
  access_mode TEXT NOT NULL DEFAULT 'read-only',
  pool_json TEXT NOT NULL DEFAULT '{}',
  init_sql_json TEXT NOT NULL DEFAULT '[]',
  color TEXT, tags_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);

CREATE TABLE execution (
  id TEXT PRIMARY KEY, connection_id TEXT NOT NULL,
  session_id TEXT, initiator TEXT NOT NULL, initiator_id TEXT,
  sql_text TEXT NOT NULL, sql_hash TEXT NOT NULL, risk TEXT NOT NULL,
  started_at TEXT NOT NULL, duration_ms INTEGER NOT NULL,
  rows_affected INTEGER, rows_returned INTEGER,
  status TEXT NOT NULL, error_json TEXT, approval_id TEXT
);
CREATE INDEX idx_exec_conn_time ON execution(connection_id, started_at DESC);

CREATE TABLE change_plan (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, connection_id TEXT NOT NULL,
  target_json TEXT NOT NULL, statements_json TEXT NOT NULL,
  rollback_json TEXT, impact_json TEXT NOT NULL, risk TEXT NOT NULL,
  status TEXT NOT NULL, created_at TEXT NOT NULL, applied_at TEXT, execution_id TEXT
);

CREATE TABLE audit_event (
  id TEXT PRIMARY KEY, at TEXT NOT NULL, actor TEXT NOT NULL, actor_id TEXT,
  action TEXT NOT NULL, target TEXT, risk TEXT, outcome TEXT NOT NULL,
  detail_json TEXT, prev_hash TEXT NOT NULL, hash TEXT NOT NULL
);
CREATE INDEX idx_audit_time ON audit_event(at DESC);

CREATE TABLE agent_conversation (
  id TEXT PRIMARY KEY, connection_id TEXT, permission_mode TEXT NOT NULL,
  provider TEXT, model TEXT, mode TEXT NOT NULL,
  status TEXT NOT NULL, token_usage INTEGER DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);

CREATE TABLE agent_step (
  conversation_id TEXT NOT NULL, seq INTEGER NOT NULL,
  kind TEXT NOT NULL, tool TEXT, args_json TEXT, result_digest TEXT,
  result_blob TEXT, duration_ms INTEGER, cost_tokens INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, PRIMARY KEY (conversation_id, seq)
);

CREATE TABLE task (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, connection_id TEXT NOT NULL,
  spec_json TEXT NOT NULL, progress_json TEXT, checkpoint_json TEXT,
  status TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);

CREATE TABLE workspace (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, layout_json TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE query_history (
  id TEXT PRIMARY KEY, connection_id TEXT NOT NULL, sql_text TEXT NOT NULL,
  executed_at TEXT NOT NULL, duration_ms INTEGER, ok INTEGER NOT NULL
);

-- 全文本搜索（作用于对象名与注释）
CREATE VIRTUAL TABLE object_search USING fts5(
  connection_id UNINDEXED, catalog UNINDEXED, schema_name UNINDEXED,
  kind UNINDEXED, name, comment, tokenize='unicode61'
);
```

**取舍说明**

- **不用 ORM**：表少、查询简单，SQL 直写更透明。
- **审计链式哈希**：`hash = SHA256(prev_hash || 规范化 JSON)`。只防「本地文件被悄悄改」，不防「有权限的人整段重写」——后者需要外部锚点，v1 不做。
- **不存结果集**：默认只存 SQL 与统计。开启「保存结果快照」是显式选项，且加密存于独立文件。
- **`query_history` 与 `execution` 分离**：前者是用户可见的历史（可删），后者是审计（不可删）。

---

## 5. 关键不变量汇总

| # | 不变量 | 强制位置 |
|---|---|---|
| I1 | 任何写语句在 `read-only` 连接上必须被拒 | `service/query` + `driver` 双保险 |
| I2 | `risk >= R2` 必须经过审批或显式配置豁免 | `service/change` |
| I3 | 凭据明文不得出现在 DTO、日志、审计、导出 | `infra/secret` + `model` 校验钩子 |
| I4 | `in_tx` 会话不得并发执行 | `service/query` |
| I5 | `applied` 的 plan 不可修改 | 存储层 `CHECK` + 服务层 |
| I6 | 迁移断点必须幂等（按主键区间 + 批哈希） | `service/migration` |
| I7 | 审计事件只追加 | 存储层仅暴露 `Insert` / `Query` |
| I8 | `Capabilities()` 为唯一能力判断源 | 代码评审 + 测试（禁止 `switch engine`） |
| I9 | `environment = unknown` 时策略严格度等同 `production` | `policy.Friction` |
| I10 | 环境只能由人放宽；自动检测只能收紧 | `policy.Friction` + 存储层校验 |
| I11 | 摩擦等级是纯函数 `Friction(env, risk, modifiers)`，不读外部状态 | `policy` 单测穷举 |
| I12 | 服务器身份变化必须阻断并重新确认环境 | `service/connection` |

---

## 6. 与上层的关系

- `service` 层的每个方法都在本文有对应实体与状态迁移。
- `api` 层的请求/响应 DTO 是 `model` 的子集投影，**不新增语义**。
- `agent` 的工具参数使用 `model` 的同一批 DTO，因此「AI 调用的参数」与「UI 提交的参数」结构一致——这是 C1 约束的落地方式。
