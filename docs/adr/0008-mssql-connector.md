# ADR-0008 · SQL Server 接入：go-mssqldb、自拼 DDL、SHOWPLAN_XML

- 状态：**已采纳（实现待真机）**
- 日期：2026-11-04
- 影响面：`internal/driver/mssql`（新增）、`internal/driver/dialect`（MSSQL 的 `ExplainSQL`）、`cmd/dbrover`（驱动注册）、`docs/04`

---

## 背景

`docs/13` 的 M1 门禁把 SQL Server 当作抽象的试金石（`docs/13` §M1：无原生 DDL、分页语法不同、XML 计划）。ADR-0001 只写了「8 种引擎均有纯 Go 驱动」，没有定 MSSQL 的具体做法。这个引擎有三个无法照抄其它驱动的地方：

1. **没有一键取 DDL**（`docs/04` §3.2）：PG 有 `pg_get_*def`、MySQL 有 `SHOW CREATE`，MSSQL 只能从 `sys.*` 拼。
2. **没有会话级只读开关**：PG 有 `default_transaction_read_only`、MySQL 有 `SET SESSION TRANSACTION READ ONLY`，MSSQL 只能靠权限或连接属性。
3. **执行计划是 XML 且必须独立会话**：`SET SHOWPLAN_XML ON` 之后「下一条语句只返回计划、不再执行」，同一个会话里往返会踩到上一句的状态。

---

## 决策

### 1. 驱动：`github.com/microsoft/go-mssqldb`

纯 Go、无 CGO，`CGO_ENABLED=0` 可交叉编译，与 ADR-0001 的硬约束一致。DSN 用驱动原生的 `sqlserver://user:pass@host:port?database=db`，口令只经 `url.UserPassword` 编码，不拼字符串。

**不引 `denisenkom/go-mssqldb`**：那是同一个项目的旧地址，已归档；两个都引会重复注册 `sqlserver` 驱动名。

### 2. 只读：只承诺做得到的那一层

MSSQL 没有会话级只读。本驱动做两件事，并且**在 `Health.Warnings` 里如实说明**：

- 连接串带 `ApplicationIntent=ReadOnly`——它只在 AlwaysOn 只读副本上真正生效，普通实例上只是提示；
- 剩下的靠执行门（`service`）拦截写语句。

**不假装**驱动层能挡住写操作。`Capabilities` 里也不新增「驱动只读」这种能力位，因为它是假的。

### 3. DDL 自拼，并且要能被回放

从这些目录取数并拼装：

| 片段 | 来源 |
|---|---|
| 列、类型、可空、排序规则 | `sys.columns` + `sys.types` |
| 标识列 | `sys.identity_columns`（`Seed`/`Increment`） |
| 计算列 | `sys.computed_columns`（`definition` + `is_persisted`） |
| 默认值 | `sys.default_constraints`（具名约束，不是列上的无名默认） |
| 主键 / 唯一约束 | `sys.key_constraints` + `sys.index_columns` |
| 普通索引 | `sys.indexes`（`is_primary_key=0 AND is_unique_constraint=0`） |
| 外键 | `sys.foreign_keys` + `sys.foreign_key_columns` |
| 检查约束 | `sys.check_constraints` |

**验收口径**：拼出的 DDL 必须能在一个空库里回放并产生等价结构（`drivertest` 的 DDL 往返用例），否则等于没实现。

### 4. 执行计划：独立会话 + XML

`Dialect.ExplainSQL` 对 MSSQL 返回 `SET SHOWPLAN_XML ON; <sql>`，但**执行方必须用一条独占连接**：先 `ON`、再发语句、再 `OFF`，三步在同一物理连接上，用完无论成败都要 `OFF`。解析走 `encoding/xml`，映射到统一的 `model.PlanNode`（NodeType、Object、Cost、Rows、Children）。

`SET SHOWPLAN_XML` 不会真的执行语句，所以这里安全；`SET STATISTICS XML` 会执行，**不使用**。

### 5. 取消：ctx 取消即发 attention

`go-mssqldb` 在 ctx 取消时向服务器发 attention packet，通常 1 秒内生效。驱动不再额外发 `KILL`：`KILL` 需要 `ALTER ANY CONNECTION` 权限，而 attention 对当前会话足够。

---

## 后果

**正面**

- MSSQL 接入不改上层：`service`/`api` 一行不动，靠 `Capabilities` 与方言分派（ADR-0005）。
- 只读的边界被写清楚，不会被误当成「驱动层已强制」。

**负面 / 需要承担**

- 自拼 DDL 覆盖不到的部分要显式说「不支持」而不是输出半截语句：稀疏列、时态表、内存优化表第一版不进 DDL 还原，遇到时返回带说明的错误。
- 计划解析只取常用字段，不做代价模型。

---

## 当前状态（2026-11-04）

**驱动未实现，代码未落地**——本 ADR 只把决策定下来，避免实现时反复讨论。原因是拿不到可验证的实例，而不是设计未定：

- `mcr.microsoft.com` 的 registry API 可达（`/v2/` 返回 200），但 blob 下载走 `southeastasia.data.mcr.microsoft.com`，实测约 85 KB/s，随后被 `connection reset by peer` 掐断；镜像约 1.5 GB，按这个链路需要数小时且会反复中断。
- Docker Hub（`registry-1.docker.io`）在这台机器上直接连接被拒，`gvenzl/oracle-free` 同样拿不到。

**验收口径不放松**：`docs/AGENTS` 要求连接器类改动有真实实例验证记录，所以不写「没跑过任何一条语句」的 MSSQL 驱动。等拿到能拉镜像的机器（或 CDN 恢复）再按本 ADR 实现，并通过 `drivertest` 全套。
