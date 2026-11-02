# 00 · 术语表与概念层级

统一词汇是跨引擎工具的第一道地基。同一个词在 8 个引擎里含义不同，本文件定义 DBRover 内部**唯一解释**。

---

## 1. 层级词汇

### 1.1 命名空间层级

不同引擎对「库」和「模式」的概念互相错位，DBRover 统一为四层：

```
Engine (引擎类型)
└─ Server (连接目标：host:port + 实例)
   └─ Catalog (目录)
      └─ Schema (模式)
         └─ Object (对象: 表/视图/例程/序列/类型/集合/索引)
            └─ Column / Field
```

各引擎映射：

| 引擎 | Catalog 对应 | Schema 对应 | 备注 |
|---|---|---|---|
| PostgreSQL | database | schema（`public` 等） | 跨 database 查询需 dblink/FDW，UI 必须限定单库 |
| MySQL | ○ 无独立层 | database ≡ schema | 两层塌缩为一层，DBRover 内部把 database 同时填入 catalog 与 schema |
| MariaDB | 同 MySQL | 同 MySQL | — |
| SQL Server | database | schema（`dbo` 等） | 四段式 `server.db.schema.obj`，跨库可查询 |
| Oracle | ○ 无 catalog | schema ≡ 用户 | 跨 schema 访问靠权限与同义词；`SID`/`Service Name` 区别必须在连接表单体现 |
| SQLite | file | `main` / `temp` / attached | 一个文件一库，attach 后多 catalog |
| Redis | db index (0-15) | ○ 无 | 逻辑库即 catalog，用 `SELECT n` 切换 |
| MongoDB | database | ○ 无（集合平铺） | 无模式，字段靠采样推断 |
| Elasticsearch | ○ 无 | index | 无 catalog，index ≡ 集合；别名与数据流需额外建模 |

> **设计后果**：UI 永远渲染四层面包屑，但**只展示该引擎存在的层**。禁止在 UI 里硬编码「schema」概念。

### 1.2 对象词汇

| DBRover 术语 | 含义 | 各引擎对应 |
|---|---|---|
| **Table** | 有行列表结构 | table / collection / index(ES) |
| **View** | 命名查询 | view / 物化视图(单独建模) / Mongo view / ES alias |
| **Routine** | 存储过程或函数 | procedure / function / PG function+routine / JS UDF(ES 部分) |
| **Trigger** | 事件驱动的副作用 | trigger / change stream(不算) |
| **Sequence** | 自增序列 | sequence / identity / auto_increment |
| **Type** | 用户定义类型 | PG composite/enum/domain、Oracle object type、SQLServer UDT |
| **Index** | 加速结构 | index / 复合索引 / ES 的 mapping+settings 组合 |

---

## 2. 操作词汇

| 术语 | 定义 | 注意 |
|---|---|---|
| **Query** | 返回结果集的语句 | 必须可判定「是否返回行」 |
| **Execute** | 不返回结果集的语句（DML/DDL） | 返回影响行数或受影响对象 |
| **Statement** | 一次发送给引擎的最小单元 | 一个脚本可含多 statement |
| **Batch** | 一次提交执行的一组 statement | 事务边界由引擎与用户选择共同决定 |
| **Execution** | 一次 Batch 的执行记录（审计单位） | 含耗时、行数、错误、发起者 |
| **Plan** | 引擎给出的执行计划 | 结构因引擎而异，需归一化 |
| **Change Plan** | DBRover 生成的变更预案（DDL/DML 预览） | 含预估影响与回滚语句 |
| **Keyset Pagination** | 基于排序键的游标分页 | 大表首选，避免深 OFFSET |

---

## 3. 权限与安全词汇

| 术语 | 定义 |
|---|---|
| **Access Mode** | 连接级权限声明：`read-only` / `read-write` / `admin` |
| **Risk Level** | 单条 statement 的风险分级：`R0` 只读 … `R4` 破坏性 |
| **Guard** | 在协议层与 SQL 分析层双重拦截的执行前检查 |
| **Approval** | 高风险操作的人工确认记录，含确认人、时间、原始 SQL 摘要 |
| **Audit Event** | 不可变执行记录，含发起者类型（human / agent / schedule） |
| **Credential Handle** | 指向 OS 凭据库条目的不透明标识，非密码本身 |

### 风险分级（全产品统一口径）

| 级别 | 判定 | 默认行为 |
|---|---|---|
| `R0` | 纯读取：`SELECT` / `SHOW` / `EXPLAIN` / `DESC` / `PING` | 直接执行 |
| `R1` | 单行/小批量 DML，命中主键或唯一索引，`LIMIT 1` 或 `WHERE` 为等值 | 直接执行（只读连接则拦截） |
| `R2` | 无界或大范围 DML（`UPDATE`/`DELETE` 无 `WHERE` 或条件非索引） | 需确认，显示预估影响行数 |
| `R3` | DDL：`CREATE` / `ALTER` / `DROP INDEX` / `TRUNCATE` | 需确认 + 展示回滚语句 |
| `R4` | 不可逆或跨对象破坏：`DROP TABLE/DATABASE`、`ALTER SYSTEM`、`FLUSHALL`、`DELETE` 无 `WHERE`、索引重建于生产大表 | 需二次确认 + 输入对象名确认 |

> 分级实现见 [`11-ai-agents.md`](11-ai-agents.md#6-风险分级器) 与 [`10-connection-and-security.md`](10-connection-and-security.md#4-只读守卫)。

---

## 4. 产品词汇

| 术语 | 定义 |
|---|---|
| **Workspace** | 一组连接 + 打开的编辑器 + 布局的持久化集合 |
| **Connection** | 一个可复用的连接配置（含隧道、凭据句柄、访问模式） |
| **Session** | 一个物理连接上的逻辑上下文（含事务、临时设置、当前 catalog） |
| **Query Tab** | 编辑器中的一个 SQL 文本单元，绑定到某个 Session |
| **Result Set** | 一次 Query 的返回，分页加载，持有可取消的游标 |
| **Agent Conversation** | 人与 AI 的一次会话，含计划、工具调用、审批 |
| **Agent Step** | 会话中的一步：思考 / 工具调用 / 观察 / 结论 |
| **Tool** | Agent 可调用的原子能力，与 `service` 方法一一对应 |
| **Runbook** | 可复用的多步任务模板（如「巡检慢 SQL 并出报告」） |

---

## 5. 易混淆点（写进代码注释与 UI 文案）

1. **MySQL 的 database 就是 schema**。UI 上不要同时展示「库」和「模式」两个下拉框。
2. **Oracle 的 schema 等于用户名**。切换 schema 等于切换用户上下文，不是切目录。
3. **PostgreSQL 不能跨 database 查询**（除非 FDW）。UI 在执行前应拦住跨库引用并给出解释。
4. **SQLite 的 catalog 是文件**。`:memory:` 与临时库需要特殊标记，不能当持久连接。
5. **Redis 的 16 个 db 是同一个实例内的逻辑分区**，`KEYS` 在集群模式下不可用。
6. **MongoDB 无 schema**：DBRover 展示的「字段」是**采样推断**结果，必须标注为推断而非定义。
7. **Elasticsearch 的 index 不是表**：mapping 变更多数不可逆，`reindex` 才是正解。
8. **SQL Server 的 `dbo` 是默认 schema**，不是用户；`sa` 才是登录名。
9. **执行计划的「成本」不可跨引擎比较**，也不同引擎版本可比。UI 只做相对高亮，不给绝对数值判断。

---

## 6. 缩写

`DDL` 数据定义语言 · `DML` 数据操纵语言 · `DCL` 权限控制 · `CDC` 变更数据捕获 · `MCP` Model Context Protocol · `SSH` 安全外壳 · `TLS` 传输层安全 · `RBAC` 基于角色的访问控制 · `ADR` 架构决策记录 · `NFR` 非功能需求 · `FR` 功能需求
