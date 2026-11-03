# 04 · 连接器层

> 8 种引擎的差异必须**收敛在一个接口后面**，而不是散落在 UI 的 `if engine ==` 里。本文是该接口的契约。

---

## 1. 设计原则

1. **能力声明优先于类型判断**：任何「某引擎不支持 X」都通过 `Capabilities()` 表达，禁止上层 `switch engine`。
2. **中立模型出站**：驱动返回 `model.*`，引擎私有结构不得越过 `driver` 边界。
3. **保留原始**：每个中立字段旁必须能取到引擎原始表示（原始类型串、原始错误码、原始计划文本），因为精确还原比统一更重要。
4. **失败要可解释**：驱动必须把引擎错误归类并附可执行建议，而不是只透传错误文本。
5. **可测试**：每个实现必须通过统一的 `drivertest` 套件；能力声明与实现不符视为缺陷。

---

## 2. 接口契约

### 2.1 主体（所有引擎必须实现）

```go
package driver

type Connector interface {
    Engine() Engine
    Capabilities() Capabilities

    // 生命周期。Open 只建立池与隧道，不保证可用；可用性由 Health 判断。
    Open(ctx context.Context, cfg ConnectConfig) error
    Close() error
    Health(ctx context.Context) (*HealthInfo, error)   // 版本、时区、只读状态、权限摘要

    // 元数据
    Meta() MetadataProvider

    // 借一条物理连接。事务必须钉在同一条上。
    Acquire(ctx context.Context) (Session, error)
    PoolStats() PoolStats
}

type Session interface {
    Query(ctx context.Context, sql string, args []any, opt QueryOptions) (*ResultCursor, error)
    Execute(ctx context.Context, sql string, args []any) (*ExecResult, error)

    Begin(ctx context.Context, opt TxOptions) error
    Commit(ctx context.Context) error
    Rollback(ctx context.Context) error
    TxState() TxState

    // 会话上下文切换（不同引擎语义不同，见 3.1）
    Use(ctx context.Context, path NamespacePath) error

    // 取消当前在途语句。必须能在语句阻塞时生效。
    Cancel(ctx context.Context) error

    Close() error
}
```

### 2.2 元数据

```go
type MetadataProvider interface {
    Catalogs(ctx) ([]CatalogRef, error)
    Schemas(ctx, catalog string) ([]SchemaRef, error)
    Objects(ctx, schema NamespacePath, kinds ...ObjectKind) ([]ObjectRef, error)

    DescribeTable(ctx, ref ObjectRef) (*TableDetail, error)
    DescribeRoutine(ctx, ref ObjectRef) (*RoutineDetail, error)
    DescribeIndex(ctx, ref ObjectRef) (*IndexDetail, error)

    DDL(ctx, ref ObjectRef) (string, error)             // 引擎原生 DDL 文本
    SearchObjects(ctx, q SearchQuery) ([]ObjectRef, error)
    Stats(ctx, ref ObjectRef) (*ObjectStats, error)     // 行数估算、体积
}
```

**缺失对象的契约**：`Describe*` 系列遇到不存在的对象必须返回包装了 `driver.ErrObjectNotFound` 的错误，**不得**返回一个零列的空结构。

这条不是洁癖：`PRAGMA table_info`、`information_schema`、`pg_catalog` 对不存在的表都只是「查不到行」而不报错，一不小心就会把「表不存在」说成「表没有列」。上层会据此给出误导性提示（例如把建表失败说成「没有主键，无法编辑」），API 也会对已删除的表返回 200 空结构。由 `drivertest` 的 `DescribeMissingObjectErrors` 用例锁住。

### 2.3 非关系型引擎：另一族接口

Redis / Mongo / ES **不实现**上面的 `Connector` 与 `MetadataProvider`。它们没有「表 / 列 / SQL」这套语义，硬套只会得到一堆永远返空的实现，让上层误判能力存在（详细论证与备选方案见 [ADR-0007](adr/0007-nosql-connector-model.md)）。

```go
// KVConnector 由 key-value 引擎实现。独立接口，不实现 Connector。
type KVConnector interface {
    Engine() model.Engine
    Capabilities() model.Capabilities
    Open(ctx context.Context, cfg ConnectConfig) error
    Close() error
    Health(ctx context.Context) (*HealthInfo, error)

    Scan(ctx context.Context, opt KVScanOptions) (*KVPage, error)      // 禁止实现成 KEYS
    Inspect(ctx context.Context, key string) (*KVEntry, error)
    Value(ctx context.Context, key string, opt KVValueOptions) (*KVValue, error)
    Mutate(ctx context.Context, op KVMutation) (*KVResult, error)
    Command(ctx context.Context, name string, args []any) (*KVResult, error)
}
```

**用哪个接口由类型断言决定，不由引擎名判断**：`api` 里 `if kv, ok := e.drv.(driver.KVConnector); ok { … }`。这与 ADR-0005 同一条原则——新增 NoSQL 引擎时 `api` 一行都不用改。

写路径复用同一套执行门、审计与摩擦分级，但**命令风险表分开**：Redis 命令没有语句结构，不能过 `dialect` 分级（表见 ADR-0007 §3，不在表里的命令按 `R4` 处理）。

### 2.4 可选能力（用能力接口 + 类型断言）

```go
type MonitorProvider interface {
    ServerMetrics(ctx) (*ServerMetrics, error)
    Sessions(ctx, filter SessionFilter) ([]DbSession, error)
    KillSession(ctx, id string) error
    SlowQueries(ctx, window time.Duration) ([]SlowQuery, error)
    LockGraph(ctx) (*LockGraph, error)
    TableSizes(ctx, limit int) ([]TableSize, error)
}

type Backuper interface {
    Backup(ctx, spec BackupSpec, sink Sink) (*BackupResult, error)
    Restore(ctx, src Source, spec RestoreSpec) (*RestoreResult, error)
}

type BulkLoader interface {
    BulkLoad(ctx, target ObjectRef, rows RowStream) (*LoadResult, error)
}

type DDLLocker interface {
    // 变更前的可行性探测：锁风险、在线 DDL 支持、预估时长
    AssessDDL(ctx, stmts []string) (*DDLAssessment, error)
}
```

上层用法：

```go
if m, ok := conn.(driver.MonitorProvider); ok && conn.Capabilities().SessionMgmt {
    // 渲染会话面板
}
```

**两层判断的理由**：`Capabilities()` 是给 UI 的「要不要显示」；类型断言是给代码的「能不能调」。两者必须一致，由 `drivertest` 断言。

---

## 3. 能力矩阵（详细）

图例：`●` 原生支持 · `◐` 支持但有约束/需降级 · `○` 不支持

### 3.1 命名空间与会话

| 项 | PG | MySQL | MSSQL | Oracle | SQLite | Redis | Mongo | ES |
|---|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|
| Catalog 层 | database | ○ | database | ○ | 文件 | db index | database | ○ |
| Schema 层 | schema | database | schema | user | main/temp | ○ | ○ | index |
| 跨 catalog 查询 | ○（需 FDW） | ◐ 同实例可 `db.tbl` | ● 三段式 | ● 同义词/权限 | ● attach | ○ | ○ | ○ |
| 切换上下文语句 | `SET search_path` | `USE db` | `USE db` | `ALTER SESSION SET CURRENT_SCHEMA` | `ATTACH` | `SELECT n` | `use db` | ○ |
| 只读强制手段 | `SET default_transaction_read_only` | `SET SESSION TRANSACTION READ ONLY` | 事务隔离+权限 | `SET TRANSACTION READ ONLY` | `PRAGMA query_only` | 连接层拦截 | 连接层拦截 | 连接层拦截 |

### 3.2 元数据

| 项 | PG | MySQL | MSSQL | Oracle | SQLite | Redis | Mongo | ES |
|---|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|
| 系统目录来源 | `pg_catalog` | `information_schema` + `SHOW` | `sys.*` + `INFORMATION_SCHEMA` | `ALL_*`/`USER_*` 视图 | `PRAGMA table_info` 等 | `TYPE`/`OBJECT ENCODING` | 采样 + `$collStats` | `_mapping` |
| 表/列注释 | ● | ● | ● | ● | ○ | ○ | ○ | ◐ mapping meta |
| 索引详情 | ● | ● | ● | ● | ● | ○ | ● | ◐ settings |
| 外键 | ● | ● | ● | ● | ● | ○ | ○ | ○ |
| 分区 | ● | ● | ● | ● | ○ | ○ | ◐ 分片 | ◐ 分片 |
| 例程源码 | ● | ● | ● | ●（可能被 wrap） | ◐ | ○ | ◐ JS | ○ |
| 对象 DDL 还原 | `pg_get_*def` | `SHOW CREATE` | 自拼（无原生） | `DBMS_METADATA` | 自拼 | 自拼 | 自拼 | 自拼 |

> **MSSQL 与 SQLite 没有「一键取 DDL」**，必须由驱动自己拼装。这是两个容易低估工作量的实现点，需专门写测试（拼出的 DDL 能被回放到新库并产生等价结构）。

### 3.3 查询与分页

| 项 | PG | MySQL | MSSQL | Oracle | SQLite | Redis | Mongo | ES |
|---|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|
| 流式读取 | ● `DECLARE CURSOR` | ● `RowStream` | ● | ● | ● | ◐ `SCAN` | ● cursor | ● `scroll`/`search_after` |
| Keyset 分页 | ● `ctid`/PK | ● PK | ● PK | ● `ROWID`/PK | ● `rowid` | ○ | ● `_id` | ● `search_after` |
| OFFSET 兜底 | ● | ● | ● `OFFSET..FETCH` | ● 12c+ / `ROWNUM` | ● | ○ | ○ | ○（`from` 有 1 万上限） |
| 总数估算 | ◐ `reltuples` | ◐ `TABLE_ROWS` | ◐ DMV | ◐ `NUM_ROWS` | ○（需 `COUNT`） | ● `DBSIZE` | ◐ `estimatedDocumentCount` | ● `_count` |
| 多语句批 | ● | ◐ 默认关闭 | ● | ○（需 PL/SQL 块） | ● | ○ | ○ | ○ |

**分页决策树**

```
if Capabilities.KeysetPagination && 表有可用排序键(PK/唯一索引/ctid/rowid):
    keyset(last_key)          # 深分页 O(log n)
else if 引擎支持 OFFSET:
    offset(page * size)       # 深分页受限，超过阈值提示"请加排序键"
else:
    单页 + 禁止翻页
```

> 深分页阈值：`offset > 10000` 时 UI 提示「当前使用偏移分页，可能变慢；建议按主键排序」。

### 3.4 执行计划

| 项 | PG | MySQL | MSSQL | Oracle | SQLite |
|---|---|---|---|---|---|
| 语句 | `EXPLAIN (FORMAT JSON, ANALYZE)` | `EXPLAIN FORMAT=JSON` / `EXPLAIN ANALYZE`(8.0.18+) | `SET SHOWPLAN_XML ON` + 执行 | `EXPLAIN PLAN FOR` + `DBMS_XPLAN.DISPLAY` | `EXPLAIN QUERY PLAN` |
| 输出 | JSON | JSON | XML | 文本表格 | 文本行 |
| 解析成本 | 低 | 低 | 中（XML） | 高（文本对齐解析） | 中（文本） |
| 真实执行 | `ANALYZE` | `ANALYZE` | `SET STATISTICS XML ON` | `DISPLAY_CURSOR`（需先执行） | 无（仅估算） |
| 注意 | `ANALYZE` 会真实执行，写语句危险 | 同左 | 同左 | 同左 | — |

**统一计划模型**

```go
type PlanNode struct {
    ID          int
    Operation   string        // 归一化：SeqScan / IndexScan / HashJoin / Sort / Aggregate ...
    Raw         string        // 原始操作名
    Relation    string
    Index       string
    EstRows     float64
    ActualRows  float64
    EstCost     float64
    ActualTime  time.Duration
    Children    []*PlanNode
    Warnings    []string      // 如 "rows estimate off by 1000x"
    Extra       map[string]string
}
```

**分析规则（跨引擎一致）**：
- 标记 `ActualRows / EstRows` 偏差 > 100 倍的节点（统计信息过期）。
- 标记全表扫描且 `ActualRows > 10000` 的节点。
- 标记 `Nested Loop` 且内层 `ActualRows > 10000`（笛卡尔风险）。
- 按 `ActualTime` 降序给出「耗时占比 Top 5」。

> 不做「成本打分」——不同引擎、不同版本的成本单位不可比。只做**相对排序**与**规则告警**。

### 3.5 监控与运维

| 项 | PG | MySQL | MSSQL | Oracle | SQLite | Redis | Mongo | ES |
|---|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|
| 会话列表 | `pg_stat_activity` | `SHOW PROCESSLIST` | `sys.dm_exec_sessions`+`requests` | `v$session` | ○ | `CLIENT LIST` | `$currentOp` | `_tasks` |
| Kill | `pg_terminate_backend` | `KILL` | `KILL` | `ALTER SYSTEM KILL SESSION` | ○ | `CLIENT KILL` | `killOp` | ◐ 取消任务 |
| 慢查询来源 | `pg_stat_statements`（需扩展） | `performance_schema.events_statements_summary_by_digest` | Query Store（2016+） | `v$sql` + `v$sqlstats`（AWR 需 License） | ○ | `SLOWLOG GET` | Profiler / `$collStats` | 慢日志配置 + `_nodes/stats` |
| 锁等待 | `pg_locks` + `pg_blocking_pids` | `performance_schema.data_lock_waits` | `sys.dm_tran_locks` + `dm_os_waiting_tasks` | `v$lock` + `v$session.blocking_session` | ○ | ○ | ○ | ○ |
| 表体积 | `pg_total_relation_size` | `information_schema.TABLES` | `dm_db_partition_stats` | `dba_segments` | `PRAGMA page_count` | `MEMORY USAGE` | `$collStats` | `_cat/indices` |
| 需额外许可 | — | — | — | **AWR/ASH 需 Diagnostics Pack** | — | — | — | ◐ 部分监控特性 |

**Oracle 许可约束很重要**：AWR/ASH 属于付费选件。默认实现必须只用 `v$`、`v$sql` 等免费视图；检测到 AWR 不可用时静默降级并在 UI 注明，**不得假设可用**。

### 3.6 备份与迁移

| 项 | PG | MySQL | MSSQL | Oracle | SQLite | Redis | Mongo | ES |
|---|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|
| 逻辑备份 | `COPY TO` / 自拼 SQL | `SELECT ... INTO OUTFILE` 受权限限 / 流式读 | 流式读 | 流式读 | `.backup` / `VACUUM INTO` | `DUMP`/`RESTORE` 自行序列化 | BSON 遍历 | `_reindex` 或 scroll 导出 |
| 原生工具 | `pg_dump`(外部) | `mysqldump`(外部) | `BACKUP DATABASE` | Data Pump | — | `BGREWRITEAOF` | `mongodump` | Snapshot API |
| 本产品策略 | **自实现流式导出**，不依赖外部二进制 | 同左 | 同左 | 同左 | 复制文件（需停机或 WAL 检查点） | 序列化命令 | BSON | NDJSON |
| 一致性保证 | `REPEATABLE READ` 快照 | `--single-transaction` 等价 | 快照隔离 | `AS OF SCN` | 文件锁 | `BGSAVE` 后读 RDB | 快照读 | 快照仓库 |

**决策：不 shell 调用 `pg_dump`/`mysqldump`**。

理由：跨平台二进制依赖、版本兼容、无法嵌入进度与断点、无法在服务化模式下使用。代价是要自己实现各引擎的导出格式，但换来一致性、可续跑、可加密、可在容器内运行。产物格式采用**自有格式（`DBR`）**或标准 SQL/NDJSON，见 08 文档。

---

## 4. 方言层

`driver/dialect` 提供纯函数，无状态、可单测：

```go
type Dialect interface {
    // 标识符
    QuoteIdent(name string) string
    QuoteQualified(path NamespacePath) string
    CaseFold(name string) string          // PG 小写、Oracle 大写、MySQL 保留

    // 分页
    PageClause(kind PageKind, size int, cursor Cursor) (sql string, args []any)

    // 类型
    ToNeutral(raw string) model.ColumnType
    FromNeutral(t model.ColumnType, target Engine) (string, []string)  // 返回 DDL 类型与警告

    // 语句
    Normalize(sql string) string           // 指纹：去字面量、统一空白
    SplitStatements(script string) ([]Statement, error)   // 需正确跳过字符串/注释/$$ 块
    Classify(sql string) StatementClass    // query | dml | ddl | dcl | tcl | unknown

    // 计划
    ParsePlan(raw []byte) (*model.PlanNode, error)
}
```

### 4.1 标识符引用（最容易出错的地方）

| 引擎 | 规则 | 陷阱 |
|---|---|---|
| PG | `"Name"`，未引用则折叠为小写 | 建表用 `"User"` 后查询必须带引号，否则找不到 |
| MySQL | `` `Name` ``，反引号可含 `\`` 需转义为 ` `` ` | `ANSI_QUOTES` 模式下 `"` 也可用，不要依赖 |
| MSSQL | `[Name]` 或 `"Name"`，取决于 `QUOTED_IDENTIFIER` | `]` 需转义为 `]]` |
| Oracle | `"NAME"`，未引用则折叠为**大写** | 小写带引号的对象名是常见事故源 |
| SQLite | `"Name"` / `[Name]` / `` `Name` `` 均可 | 大小写不敏感（ASCII 范围） |
| Redis | 无标识符概念 | key 是二进制安全字符串 |
| Mongo | `db.collection`，集合名可含 `.` | `system.` 前缀保留 |
| ES | index 名限小写、不可含 `\ / * ? " < > | , #` | 校验必须在创建前做 |

**强制要求**：所有由用户输入拼入 SQL 的标识符必须经 `QuoteIdent`。代码里出现 `fmt.Sprintf("... %s ...", userInput)` 且不是 `QuoteIdent` 结果，一律视为缺陷。

### 4.2 语句切分

`SplitStatements` 必须正确处理：

- 单引号字符串内的 `;`，含 `''` 与 `\'` 转义（各引擎不同）。
- 双引号/反引号内的 `;`。
- 行注释 `--`、`#`（MySQL）、块注释 `/* */`（可嵌套？PG 支持嵌套，MySQL 不支持）。
- PG 的 dollar-quoted 字符串 `$$...$$`、`$tag$...$tag$`。
- Oracle 的 `/` 单独一行作为 PL/SQL 块结束符。
- MSSQL 的 `GO` 批分隔符（不在 `BEGIN...END` 内）。
- DELIMITER 变更（MySQL 客户端指令，服务端不认识——需在客户端解析并剥离）。

**这是必须有大量表驱动测试的模块**。错误切分会把一条语句拆成两条，产生难以理解的语法错误。

### 4.3 归一化（SQL 指纹）

目标：把 `SELECT * FROM t WHERE id = 123` 与 `... id = 456` 归为同一指纹。

规则：
1. 字符串字面量 → `?`
2. 数字字面量 → `?`
3. `IN (1,2,3)` → `IN (?+)`
4. 空白折叠为单空格，关键字大写
5. 移除注释
6. 保留标识符原样（不折叠）

输出 `SHA256(normalized)` 作为 `sql_hash`。用于慢查询聚合与历史去重。

---

## 5. 各引擎实现要点与坑

### 5.1 PostgreSQL

- 驱动用原生 `pgx/v5`，**不走 `database/sql`**（需要 `CopyFrom`、`LISTEN`、取消请求、批量协议）。
- 大结果集：`BEGIN; DECLARE c CURSOR FOR ...; FETCH FORWARD n` 才能流式；普通 `Query` 会缓冲。
- 取消：`pgx` 的 `ctx` 取消会发送 CancelRequest，需独立的短连接，注意别在事务中误用。
- `default_transaction_read_only=on` 是只读连接的最强保障，优先于 SQL 文本分析。
- `pg_stat_statements` 默认未装：需检测并在 UI 提示「安装扩展以获得慢查询统计」，给出 `CREATE EXTENSION` 语句。
- DDL 事务性：支持，可回滚——**这是 PG 相对 MySQL 的巨大优势**，ChangePlan 的回滚是真实的。
- 类型：`jsonb`（不是 `json`）、数组、`enum`、`domain`、`composite`、`range`、`vector`（pgvector）。渲染时逐个处理，不能一律当字符串。
- `reltuples = -1` 表示从未 analyze，估算不可信。

### 5.2 MySQL / MariaDB

- `information_schema` 在数万表实例上很慢（8.0 有所改善）。优先用 `SHOW` 系列与 `performance_schema`，并对元数据做强缓存。
- **DDL 隐式提交且不可回滚**。因此：
  - `ChangePlan.rollback` 对 DDL 只能是「补偿语句」（如 `DROP COLUMN` 反向加回），且**不保证数据恢复**。必须显式标记 `lossy_rollback: true`。
  - `DDLTransactional = false`。
- 深分页 `LIMIT 100000, 20` 会扫描 10 万行。强制 keyset。
- 取消查询需另开连接执行 `KILL QUERY <thread_id>`，因为原连接在阻塞。
- 慢查询优先 `performance_schema.events_statements_summary_by_digest`（结构化）而非 slow log 文件（需文件权限）。
- `sql_mode` 影响严格性：`STRICT_TRANS_TABLES` 缺失时插入会静默截断。连接后读取并展示。
- 默认隔离级别 `REPEATABLE READ`（与 PG/MSSQL/Oracle 的 `READ COMMITTED` 不同），UI 在事务面板显式提示。
- 字符集与排序规则：`utf8mb4_0900_ai_ci` vs `utf8mb4_general_ci` 会影响唯一索引判重与排序，跨库迁移必须比对。
- MariaDB 差异：`JSON` 是 `LONGTEXT` 别名、无 `performance_schema.data_lock_waits`（用 `information_schema.INNODB_LOCKS`）、`SEQUENCE` 支持但语法不同。需独立 `Capabilities`。

### 5.3 SQL Server

- 四段式 `[server].[db].[schema].[obj]`；跨库查询默认可用，但 `cross db ownership chaining` 影响权限。
- 取执行计划：`SET SHOWPLAN_XML ON` 后**下一条语句不执行只返回计划**，必须用独立会话，且记得 `OFF`。
- 取消：`go-mssqldb` 的 `ctx` 取消会发 attention packet，通常 1s 内生效。
- 慢查询最佳来源是 **Query Store**（2016+），需检测是否启用；否则退化为 `sys.dm_exec_query_stats`。
- 无「一键取 DDL」：必须从 `sys.columns`/`sys.indexes`/`sys.foreign_keys`/`sys.default_constraints` 拼装。注意：
  - `IDENTITY` 属性在 `sys.identity_columns`
  - 计算列在 `sys.computed_columns`
  - 稀疏列、时态表、内存优化表都要单独处理
- 分页用 `ORDER BY ... OFFSET n ROWS FETCH NEXT m ROWS ONLY`（2012+），无 `ORDER BY` 不能分页。
- `KILL` 需要 `ALTER ANY CONNECTION` 类权限；无权限时给出明确提示。

### 5.4 Oracle

- **连接串**：`SID` 与 `Service Name` 是两种东西，表单必须让用户选。go-ora 的 DSN 格式不同，需转换层。
- **schema == user**：切 schema 是 `ALTER SESSION SET CURRENT_SCHEMA`，切换后未限定名的对象解析到该 schema，但权限仍按登录用户。
- **空字符串等于 NULL**：`'' IS NULL` 为真。这与所有其他引擎相反，UI 与迁移必须显式处理，否则会把 `''` 迁成 `NULL`。
- **未引用标识符折叠为大写**：`create table t` → `T`。驱动内部统一用「实际存储名」，不做二次折叠。
- **`DATE` 含时分秒**（不是纯日期）；`TIMESTAMP` 才有小数秒与时区变体。
- 无 `LIMIT`：12c+ 用 `FETCH FIRST n ROWS ONLY`；更早版本用 `ROWNUM` 包裹（注意 `ORDER BY` 与 `ROWNUM` 的执行顺序陷阱）。
- 执行计划：`EXPLAIN PLAN FOR` + `SELECT * FROM TABLE(DBMS_XPLAN.DISPLAY(NULL,NULL,'ALL'))`，输出是文本表格，解析要对齐列宽（成本高，需专门测试）。真实执行统计用 `DBMS_XPLAN.DISPLAY_CURSOR`，要求语句刚执行过且在共享池中。
- Kill 会话：`ALTER SYSTEM KILL SESSION 'sid,serial#' IMMEDIATE`，需要 `ALTER SYSTEM` 权限。
- **AWR/ASH 需 Diagnostics Pack 许可**，默认不查询；仅在检测到可用且用户显式开启时使用。
- 长事务与 UNDO 空间：大更新会撑爆 UNDO，`ChangePlan` 必须提示分批。
- PL/SQL 块以 `/` 结束，语句切分需特殊处理。

### 5.5 SQLite

- 一个文件一个库；`ATTACH` 后可跨库但仍是单文件写锁。
- 单写者模型：写事务期间其他写会收到 `SQLITE_BUSY`。驱动需设置 `busy_timeout` 并提示。
- WAL 模式下读不阻塞写，但要求文件系统支持共享内存（网络盘可能不行）。
- 无 `ALTER COLUMN`：改列类型/约束需「建新表 → 拷数据 → 删旧表 → 改名」，`ChangePlan` 必须生成这套四步并包在事务里（SQLite DDL 是事务性的，这是优点）。
- 类型是「亲和性」而非强类型：`INTEGER` 列可以存字符串（部分情况）。UI 渲染以实际值为准。
- 取 schema：`PRAGMA table_info` / `index_list` / `foreign_key_list` / `table_xinfo`（含生成列），而不是查 `sqlite_master` 解析 SQL。
- 备份：`VACUUM INTO 'file'` 是最安全的方式（在线、一致）；直接复制文件在 WAL 模式下会不一致。
- 无用户与权限：`read-only` 只能在连接层用 `?mode=ro` 实现。

### 5.6 Redis

模型与分层见 [ADR-0007](adr/0007-nosql-connector-model.md)：Redis **不实现** `Connector`/`Metadata`，改用独立的 `driver.KVConnector`；`api` 通过类型断言选择路径，不判断引擎名。

**能力契约（第一版范围）**

| 做什么 | 怎么触发 | 失败与降级 | 验收 |
|---|---|---|---|
| 浏览 keyspace | 界面左侧（按 `connection.kind=keyvalue` 自动切换）；接口是 `GET /connections/{id}/kv/keys?cursor&pattern&type&count` | 游标遍历超时 → 返回已取到的部分 + `has_more: true`，**不假装取完** | 真机 Redis 8.0.5 + 浏览器实测：6 个 key 列出、pattern 框可用、点 key 进详情 |
| 看一个 key | `GET /kv/entry?key=...` → 类型 / TTL / 编码 / 元素数 / 体积 | `MEMORY USAGE` 超时 → 只给类型与元素数，体积字段缺失，不猜 | 真机返回 `type=stream ttl=-1 size=1277`；TTL -2 表示 key 不存在 |
| 读值 | `GET /kv/value?key=...&cursor&count&max_bytes`，按类型分页（`HSCAN`/`SSCAN`/`ZRANGE`/`XRANGE`/`GETRANGE`） | string 超过 8 KB 默认只取前 8 KB 并标 `truncated: true` | 真机读 stream 返回 `pairs=0`（空队列）不报错 |
| 改值 | `POST /kv/apply`，一次一个结构化写操作（`set_string` / `set_field` / `delete_key` / `delete_item` / `push` / `expire`） | 与命令模式共用同一道门（`service.KVExecutor`）。**刻意不做「一次多条命令」的预案**：Redis 的 `MULTI/EXEC` 出错不回滚，套用关系型那套「整批回滚」文案会骗人 | `TestKVWriteGateAndApprovalBinding`；真机 Redis 8.0.5：写 string（带 TTL 120）→ 读回一致且 `ttl=120`；写 hash field → `HGETALL` 读回；`delete_key` 返回 1 且之后 `TYPE=none`；审计里三条 `kv.mutate` |
| 命令模式 | `POST /kv/command`，结构化「命令 + 参数」 | 走与 SQL 同一道门（`service.KVExecutor`：分级 → 摩擦 → 审批 → 审计）。生产环境的写命令返回 428 并签发**绑定命令指纹**的凭证；只读连接上的写命令 `403 POLICY_READ_ONLY`（与 SQL 同一条规则）；`KEYS` 与 `DEBUG` 直接 `403 POLICY_COMMAND_BLOCKED` 并说明替代方案 | `TestKVEndpointsAndPolicyGates`、`TestKVWriteGateAndApprovalBinding`（凭证换命令、换操作类型都 409）；真机 Redis 8.0.5 上 `DBSIZE` 放行、`KEYS`/`DEBUG` 被拒 |

**实现约束**

- 不是 SQL 引擎：编辑器需「命令模式」，带命令与参数的结构化校验（不做自由文本解析）。
- 集群模式（Cluster）下：`KEYS` 不可用 → 只能 `SCAN`（且需逐节点扫描）；多 key 命令要求同 slot（`{tag}` hash tag）；db index 只有 0。**第一版不支持集群**，检测到集群时 `Health` 给 `Warnings` 并在 UI 标注「集群未验证」，连接照常可用。
- 16 个逻辑库仅非集群模式；`SELECT` 切换。
- 类型化编辑器：第一版做 `string` / `hash` / `list` / `set` / `zset` / `stream` 六种内建类型。`bitmap` / `hyperloglog` / `geo` / `module` 暂按 binary 或只读展示。
- 大 key 检测：`MEMORY USAGE`（可能阻塞，需限流）+ `STRLEN`/`HLEN`/`LLEN`/`SCARD`/`ZCARD`/`XLEN`。
- 慢日志：`SLOWLOG GET n`；`SLOWLOG LEN` 先探测。
- 会话：`CLIENT LIST` 解析（7.0+ 有 `CLIENT LIST TYPE`），Kill 用 `CLIENT KILL ID <id>`。
- 值预览要限长：一个 500MB 的 string 不能整取，默认 `GETRANGE 0 8191`。
- `DEBUG` 系列命令默认禁用（危险）。
- 命令风险分级见 ADR-0007 §3：**不在表里的命令一律按 `R4` 处理**并与 SQL 侧同一条原则（未知即最危险）。

### 5.7 MongoDB

- 无 schema：列信息来自**采样**（默认 100 文档，可配）。UI 必须标注「推断」。采样要跨分片、跨索引，成本需控制。
- 类型系统：`ObjectId` / `Decimal128` / `Date`(UTC) / `Binary` / `Int64` / `Double` / `Regex` / `Timestamp` / `MinKey`/`MaxKey`。渲染与编辑都要保真，尤其是 `Decimal128`（不能转 float 再转回）。
- 查询是 MQL（JSON），不是 SQL：编辑器提供 JSON 模式 + 校验；结果以文档树展示。
- 更新语义：`updateOne` vs `replaceOne` 不同；数据网格编辑要用 `updateOne` + `$set` 精确到字段路径，避免整文档覆盖导致并发丢失。
- 执行计划：`explain("executionStats")`，重点是 `totalDocsExamined / nReturned` 比值与 `COLLSCAN`。
- 会话：`$currentOp`（需 `inprog` 权限），Kill 用 `db.killOp(opid)`。
- 变更流（Change Stream）需副本集/分片集群，单机不可用——迁移的增量方案要检测。
- 导出：自己遍历集合写 BSON 流（`Extended JSON` 作为可读备份格式），不用 `mongodump` 二进制。

### 5.8 Elasticsearch

- 无 catalog，`index` 是最上层；别名与 data stream 需要额外建模（一个别名可指向多个 index）。
- mapping 变更受限：多数类型变更需要 `reindex` + `alias` 切换。`ChangePlan` 必须生成完整流程（建新索引 → reindex → 切别名 → 删旧索引），并明确指出中间态。
- 查询是 DSL，不是 SQL（`_sql` 属付费特性范围，且功能子集）。
- 分页：`from/size` 上限 `index.max_result_window`（默认 10000）；深翻页必须用 `search_after` 或 `scroll`/`point_in_time`。
- 认证：Basic / API Key / PKI / OIDC，连接表单需支持多种。
- 慢日志需在索引级配置（`index.search.slowlog.threshold.query.warn`），驱动需读取并展示，不能假设已开。
- 集群健康与节点统计：`_cluster/health`、`_nodes/stats`、`_cat/indices`。`_cat` 系列返回文本表格，`?format=json` 更可靠。
- 备份通过 snapshot API，需先注册 repository（本地/ S3/ HDFS），属于「需要前置配置」的能力，UI 必须给出配置引导而非直接失败。

---

## 6. 一致性测试套件（`driver/drivertest`）

每个引擎实现必须通过同一套用例；能力声明与实现不一致即失败。

```
drivertest/
├─ connect_test.go        # 连通、Health、版本、时区、只读状态
├─ metadata_test.go       # 三层清单、描述表、索引、外键、DDL 往返
├─ query_test.go          # 类型保真（每类各一列）、NULL、大字段、Unicode、时区
├─ pagination_test.go     # keyset 正确性、边界、空结果、深分页
├─ dml_test.go            # 插入/更新/删除、影响行数、冲突检测
├─ ddl_test.go            # 建表→改表→回滚（按 DDLTransactional 断言不同行为）
├─ cancel_test.go         # 长查询可取消，且 1s 内生效
├─ split_test.go          # 语句切分表驱动（见 4.2 全部情形）
├─ normalize_test.go      # 指纹归一化
└─ capabilities_test.go   # 声明与类型断言一致
```

**类型保真测试是重点**：每种引擎建一张「全类型表」，写入边界值，读回后逐字段比对。历史上大量工具在 `Decimal`、`DateTime` 时区、`Binary`、`NULL vs 空串` 上出错。

---

## 7. 降级策略总表

能力缺失时 UI 与行为必须明确降级，不能静默失败：

| 缺失能力 | 降级行为 | 用户可见提示 |
|---|---|---|
| Keyset 分页 | 用 OFFSET，超过阈值警告 | 「当前使用偏移分页，可能较慢」 |
| 执行计划 | 显示原始文本 | 「该引擎仅提供文本计划」 |
| 会话 Kill | 隐藏按钮 | 「当前账号无终止会话权限」 |
| 慢查询统计 | 隐藏面板或显示「需安装扩展」 | 附安装语句 |
| 表体积统计 | 显示「—」 | 「需更高权限或额外许可」 |
| 事务性 DDL | 标记回滚为「补偿且可能丢数据」 | 红色警示 |
| 一键 DDL 还原 | 用驱动拼装的 DDL 并标注 | 「DDL 由工具生成，可能与原定义有差异」 |
| 无主键表 | 数据网格只读 | 「该表无主键，禁止编辑」 |
| 采样 schema（Mongo） | 标注推断 | 「字段基于 100 条文档采样推断」 |

**原则**：宁可少一个按钮，也不要给一个会失败的按钮。

---

## 8. 新增引擎的检查清单

- [ ] 实现 `Connector` + `Session`，`Capabilities()` 填真实值
- [ ] 实现 `MetadataProvider`，含 `DDL()` 往返测试
- [ ] 实现 `dialect.Dialect`，含 `SplitStatements` 全情形用例
- [ ] 实现 `ParsePlan`
- [ ] 可选接口按需实现，且与 `Capabilities()` 一致
- [ ] 通过 `drivertest` 全套
- [ ] 补 `testdata/docker-compose.<engine>.yml`
- [ ] 更新本文档 3.x 矩阵
- [ ] 补一条 ADR（若引入新依赖或有特殊约束，如 Oracle 许可）
