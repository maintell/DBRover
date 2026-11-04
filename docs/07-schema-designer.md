# 07 · 结构与对象管理

> 需求编号：`FR-SCH-01..08`。核心命题：**DDL 不是「填表单点保存」，而是一次有影响面的变更。**

---

## 1. 对象设计器

### 1.1 覆盖范围

| 对象 | 设计能力 | 优先级 |
|---|---|---|
| 表 | 列、索引、外键、检查约束、注释、引擎特有项 | P0 |
| 视图 | 定义 SQL、检查选项、算法、安全性 | P0 |
| 物化视图 | 定义、刷新策略（PG 支持） | P1 |
| 索引 | 独立创建/编辑（含部分索引、表达式索引、覆盖索引） | P0 |
| 序列 | 起始、步长、缓存、循环 | P1 |
| 存储过程/函数 | 参数签名、语言、返回类型、可编辑源码 | P1 |
| 触发器 | 时机、事件、粒度、函数（PG 特有） | P1 |
| 用户类型 | 枚举值、复合类型、域 | P1 |
| 集合/映射（NoSQL） | 索引、校验器、settings | P1 |

### 1.2 表单 ≠ 单一形态

表设计器分**两个视图**，同一个模型：

- **表单视图**：适合新增/小改。列以行展开，每行一组输入。
- **SQL 预览视图**：实时显示将要执行的 DDL，且可切到「手写模式」直接编辑。

**关键**：表单改动 → 立即生成 DDL → 用户能看到最终语句。**不允许「表单改了但看不见会执行什么」**。

### 1.3 引擎特有项的呈现原则

不隐藏、不伪造。表单按 `Capabilities()` 与引擎元数据渲染：

| 引擎 | 特有项 |
|---|---|
| PG | `UNLOGGED`、分区策略、`WITH (fillfactor)`、表空间、`INHERITS` |
| MySQL | `ENGINE`、`CHARSET`/`COLLATE`、`ROW_FORMAT`、`AUTO_INCREMENT`、分区 |
| MSSQL | 文件组、稀疏列、时态表（`SYSTEM_VERSIONED`）、内存优化 |
| Oracle | 表空间、`COMPRESS`、分区、`SEGMENT CREATION` |
| SQLite | `WITHOUT ROWID`、`STRICT`、`AUTOINCREMENT` |

未知项通过 `EngineExtra map[string]any` 透传，UI 以「原始属性（只读）」区块展示，避免信息丢失。

---

## 2. Change Plan（变更预案）

### 2.1 生成流程

```
表单改动 / SQL 手写 / 结构比对结果
   ↓
1. Diff 计算：期望结构 vs 当前结构
   ↓
2. 语句生成：按引擎规则产出有序 DDL
   ↓
3. 可行性探测（DDLLocker，若可用）
   · 是否需要表重建
   · 预计锁级别与时长
   · 是否可用在线 DDL / CONCURRENTLY
   · 是否存在长事务阻塞
   ↓
4. 回滚生成
   · DDLTransactional=true → 生成真实反向语句
   · false → 生成补偿语句 + 标记 lossy
   ↓
5. 风险分级（R0..R4）
   ↓
ChangePlan
```

### 2.2 影响面评估

| 指标 | 获取方式 | 精度 |
|---|---|---|
| 影响行数 | `COUNT` / 统计信息估算 | 精确或估算（标注） |
| 表体积 | `ObjectStats.SizeBytes` | 估算 |
| 锁级别 | 引擎规则表 + 实测（可选） | 规则推断 |
| 预计时长 | 基于体积与操作类型的经验模型 | 粗估，标注「经验值」 |
| 阻塞风险 | 当前是否有长事务/活跃查询 | 实时探测 |
| 依赖对象 | 引用该表的视图/例程/外键 | 精确（元数据） |

**时长预估必须标注为经验值**。给一个具体数字却不准，比给区间更糟。用「秒级 / 分钟级 / 十分钟级以上」三档。

### 2.3 各引擎 DDL 的事务性差异（必须体现在 UI）

| 引擎 | DDL 事务性 | 回滚语义 | UI 表现 |
|---|:--:|---|---|
| PostgreSQL | ✅ 支持 | **真实回滚**，DDL 失败全部撤销 | 绿色「可回滚」 |
| SQLite | ✅ 支持 | 真实回滚 | 绿色「可回滚」 |
| SQL Server | ✅ 多数支持 | 真实回滚（除少数如 `CREATE DATABASE`） | 绿色「多数可回滚」 |
| Oracle | ❌ 隐式提交 | 只能补偿 | 橙色「DDL 已提交，回滚为补偿」 |
| MySQL / MariaDB | ❌ 隐式提交 | 只能补偿，且**可能丢数据** | 红色「不可回滚，回滚可能丢数据」 |

> MySQL 上 `ALTER TABLE ... DROP COLUMN` 之后的「回滚」是 `ADD COLUMN`，**数据永久丢失**。这个提示必须显眼，不能折叠。

### 2.4 在线变更建议

当检测到高风险 DDL 时，给出具体替代语句：

| 场景 | 引擎 | 建议 |
|---|---|---|
| 加索引 | PG | `CREATE INDEX CONCURRENTLY`（**不能在事务中**，需单独提示） |
| 加索引 | MySQL 8 | `ALTER TABLE ... ADD INDEX ..., ALGORITHM=INPLACE, LOCK=NONE` |
| 加非空列 | PG 11+ | `ADD COLUMN ... DEFAULT <常量>` 不重写表 |
| 加非空列 | PG <11 | 分三步：加可空列 → 批量回填 → 设 `NOT NULL`（用 `CHECK ... NOT VALID` + `VALIDATE`） |
| 改列类型 | PG | 多数需要重写表；建议新建列 + 双写 + 切换 |
| 改列类型 | MySQL | `ALGORITHM=COPY` 会锁表；建议 gh-ost/pt-osc（外部工具，仅提示） |
| 设 NOT NULL | PG 12+ | 先加 `CHECK (col IS NOT NULL) NOT VALID` → `VALIDATE CONSTRAINT` → `SET NOT NULL`（避免全表锁） |
| 大表删除 | 任意 | 分批 + `LIMIT`，避免长事务与 UNDO 膨胀 |

**建议是「提示 + 一键插入编辑器」，不自动改写**。自动改写会在边界情况下产生错误语句。

### 2.5 预执行检查（Preconditions）

生成预案时逐条校验。**阻断与提示的边界是刻意划的**：能确定「现在做会更糟」的才阻断，其余提示——否则工具会在忙碌的库上完全没法用。

| 检查 | 手段 | 不通过时 |
|---|---|---|
| 对象仍存在且定义未变 | 比对 `DDL` 的哈希；建表要求「执行时表仍不存在」 | **阻断**（`409 SCHEMA_CHANGED`） |
| 外键相关表无孤儿数据 | `PrecheckForeignKey`（加外键前） | **阻断**（`409 FK_PRECHECK_BLOCKED` 带样本） |
| 唯一索引前无重复值 | `probeDuplicates` | **阻断**（`409 DUPLICATE_ROWS` 带样本） |
| 收窄类型不丢数据 | `probeNarrowing` | **阻断**（`409 NARROWING_BLOCKED` 带样本） |
| 目标不再被引用 | `Metadata.Dependents`（删表/删列前） | **阻断**（`409 DEPENDENCY_BLOCKED`，列出引用方） |
| 无人在等锁 | `Metadata.Blockers`：`lock_wait` 类 | **阻断**（`409 LOCK_WAIT_BLOCKED`）——已经有人在排队，再上一条拿锁的 DDL 只会让队列更长 |
| 无长事务 | `Metadata.Blockers`：`idle_in_transaction` 类（阈值 60 秒） | **只提示**（写进预案 `notes`，最多 3 条）。忙碌的库上永远有人开着事务，用它阻断等于关掉整个结构设计器 |
| 无活跃 DDL / MDL | 未做：MySQL 5.7 没有 `performance_schema.metadata_locks`，做不了就如实写在这里 | — |
| 磁盘空间充足 | 未做 | — |

**SQLite 不参与这两项**：单写者模型，写事务直接 `SQLITE_BUSY`，不存在「长事务卡住 DDL」，`Blockers` 恒为空（drivertest 明确 skip，而不是假装测过）。

---

## 3. DDL 还原（重建语句）

「查看 DDL」是高频功能，也是**实现质量差异最大**的地方。

| 引擎 | 手段 | 保真度 | 必须注意 |
|---|---|:--:|---|
| PG | `pg_get_tabledef` 无原生；用 `pg_dump --schema-only` 逻辑由库实现（拼装 `pg_catalog`） | 高 | 需处理 `INHERITS`、分区、`UNLOGGED`、权限 |
| MySQL | `SHOW CREATE TABLE` | 极高 | 返回引擎规范化的定义，可直接用 |
| MSSQL | 无原生，必须拼 `sys.*` | 中 | `IDENTITY`、计算列、稀疏列、默认约束命名、文件组、时态表 |
| Oracle | `DBMS_METADATA.GET_DDL` | 高 | 输出含存储参数与表空间，需可裁剪 |
| SQLite | 从 `sqlite_master.sql` 取原文 | 极高 | 但原文可能不规范；索引需单独取 |
| Redis | 不适用 | — | — |
| Mongo | 拼 `createIndexes` + `collMod` | 中 | 校验器、分片键需单独取 |
| ES | 拼 `PUT /index`（settings + mappings） | 高 | 需剥离不可写字段（如 `uuid`、`creation_date`） |

**验收方式（强制）**：把生成的 DDL 在一个空库中回放，再对回放后的结构做一次描述比对，必须与原结构等价。这是 `drivertest/ddl_test.go` 的核心用例，MSSQL 与 SQLite 尤其需要。

---

## 4. 结构比对

### 4.1 能力

- 两个连接之间（可跨引擎）或同连接的两个 schema 之间比对。
- 覆盖：表/列/类型/可空/默认值/注释/索引/外键/检查约束/视图定义。
- 输出三分类：**仅左侧**（删除）、**仅右侧**（新增）、**差异**（修改）。
- 生成「升级脚本」与「回退脚本」。
- 每条差异都带两侧结构原文（`left_ddl` / `right_ddl`）供人眼对照——差异清单只回答「哪一列不对」，要判断怎么改还得看整张表长什么样。

### 4.2 比对规则

- 类型比对走中立类型 + 原始类型双轨：中立类型不同必报差异；中立相同但原始不同（如 `varchar(50)` vs `varchar(100)`）也报差异并标注。
- **原始类型文本先归一化，再比对**：`character varying(n)` 与 `varchar(n)`、`integer` 与 `int` 是同一类型的两种打印方式（PostgreSQL 的 `FormatType` 与 MySQL 的 `COLUMN_TYPE` 各说各话），归一后不报差异；MySQL 的整数显示宽度（`bigint(20)`、`int(11)`）在 MySQL 8 已废弃，同样剥掉。`decimal(10,2)` 这类带两位数字的括号必须保留——那是精度不是宽度。这条不做「聪明的类型映射」，只抹平同义写法。
- 默认值比对做归一化（去外层括号、统一 `now()` 与 `CURRENT_TIMESTAMP` 视为等价需**引擎内**判断，跨引擎不视为等价）。
- 索引比对按「列序列 + 唯一性 + 条件」为键，不按索引名（名字不同不一定是差异）。
- **视图只比查询体**：从 `CREATE VIEW` 原文里取第一个位于括号外层、不在引号里的 `AS` 之后的部分（`CREATE ... VIEW x [(列清单)] AS <查询>` 的分界）。`ALGORITHM=` / `DEFINER=` / `SQL SECURITY` 这些头部是**部署属性**（谁建的、以谁的身份跑），不是结构差异，不进比对——混进来会让同一段查询在两边永远显示「不一样」。查询体只折叠空白、去掉结尾分号；**不去引号、不改大小写**（字符串字面量大小写敏感，误判「相同」比误报更危险）。
- **取不到定义时不下「相同」的结论**：任一侧的视图定义读不出来就报差异并说明是哪一侧，把「不知道」判成相同会把真差异藏起来。
- 跨引擎比对的类型差异一律标为「需人工决策」，不自动映射。
- 脚本生成按「丢了能不能拿回来」分类：删索引、删视图、删外键都不丢数据，两个方向都生成；删表、删列、改类型会丢数据，只进 `need_review`。跨引擎时右侧的 `CREATE VIEW` 原文是另一套方言写的，不直接放进升级脚本，只给原文并记 `need_review`。

### 4.3 边界

- 不做数据比对（那是迁移模块的事）。
- 不自动执行比对结果，必须经过 Change Plan。
- 大 schema（>2000 对象）分批比对并显示进度。
- **物化视图与例程（存储过程/函数）不参与比对**：物化视图的删与建不是一条 `DROP VIEW`，例程体在三种引擎里几乎没有可比性（SQLite 根本没有）。这两类目前只能靠「结构原文」人工看。

---

## 5. 依赖与安全

### 5.1 依赖检查

重命名/删除对象前，查询引用关系：

| 依赖类型 | 来源 | 精度 |
|---|---|---|
| 视图依赖 | PostgreSQL `information_schema.view_table_usage` 精确；MySQL / SQLite 回落 `view_definition` / `sqlite_master.sql` 文本匹配 | PG 精确，其余**可能误报** |
| 外键依赖 | 各引擎的 `key_column_usage` / `pg_constraint` / `PRAGMA foreign_key_list` | 精确 |
| 例程依赖 | 未做（文本搜索例程体误报率太高，等确切需求） | — |
| 应用代码依赖 | 不覆盖（超出范围） | — |

**实现口径**（ADR-0006）：

- 查询放进 `driver.Metadata.Dependents`，每项带 `Exact`。`service` 不判断引擎名。
- **视图依赖宁可多报，不可漏报**：文本匹配用词边界、大小写不敏感；`Exact=false` 的项在提示里显式标注「按文本匹配得到，可能误报」。
- **删表**：任一依赖（视图或入向外键）存在即阻断，返回 409 `DEPENDENCY_BLOCKED` 并列出引用方。
- **删列**：只按精确信息阻断——引用该列的入向外键挡住；视图依赖因为无法判断是否用到**那一列**（视图体里可能只是 `SELECT *`），不阻断，只写进预案的 `notes` 让人自查。这条是**有意的不对称**，不藏起来。

有依赖时：阻断删除，或要求「先处理依赖」并提供依赖清单。重命名时提示「依赖对象不会自动更新」。

### 5.2 危险操作的门槛

| 操作 | 风险 | 额外要求 | 测试环境 | 生产环境 |
|---|:--:|---|:--:|:--:|
| `DROP TABLE` | R4 | 输入表名确认 + 展示行数 + 建议先备份 | F3 | F4 |
| `DROP DATABASE/SCHEMA` | R4 | 输入库名确认 + 二次密码 | F3 | F4 + 禁止 Agent 发起 |
| `TRUNCATE` | R4 | 输入表名确认（MySQL 上不可回滚） | F3 | F4 |
| `DROP COLUMN` | R3 | 展示该列非空行数 + 明确「数据将丢失」 | F2 | F3 |
| 改列类型（收窄） | R3 | 预检是否有越界数据（`SELECT COUNT(*) WHERE 不满足新类型`） | F2 | F3 |
| 加外键 | R3 | 预检孤儿数据 | F2 | F3 |
| 加唯一索引 | R3 | 预检重复值 | F2 | F3 |
| `ALTER TABLE ... ENGINE=` | R3 | MySQL 上等价于整表重建 | F2 | F3 |

**收窄类型预检**示例（`bigint` → `int`）：

```sql
SELECT COUNT(*) FROM t WHERE c > 2147483647 OR c < -2147483648;
```

非零则阻断并给出样本行。

---

## 6. 视图与例程

### 6.1 视图

- 定义 SQL 用 Monaco（复用编辑器与补全）。
- 保存前做 `EXPLAIN` 空跑验证（只读，安全）。
- 展示「依赖此视图的对象」。
- MySQL 的 `ALGORITHM`/`DEFINER`/`SQL SECURITY` 作为高级项，默认不显示（避免误设 `DEFINER`）。

### 6.2 例程

- 源码编辑需注意：引擎返回的源码可能已被规范化（Oracle wrap、MySQL 去掉 `DELIMITER`）。
- 保存时**整段替换**（`CREATE OR REPLACE` 或 `DROP`+`CREATE`），不做行级 diff 应用。
- 参数签名以表格展示，且与其在源码中的定义交叉校验，不一致时提示。
- Oracle 编译错误需解析 `USER_ERRORS` 并定位到行。

---

## 7. 验收清单

| # | 项 | 判定 |
|---|---|---|
| 1 | 表设计器改动实时生成 DDL 且可见 | 手动 + 集成 ✅ 后端：`POST /schema/plan?preview=1` 只算语句**不保管预案**——`TestSchemaPreviewDoesNotStorePlan` 断言「拿预览的 id 去执行返回 404，不带 preview 的正式预案才 201 且能执行」；前端：输入变化 250ms 后调预览并展示（浏览器实测） |
| 2 | 5 种关系库的 DDL 还原可空库回放并结构等价 | `drivertest` ⚠️ **3/5**：SQLite / MySQL / PostgreSQL 的 `DDLRoundTrip` 真机通过；SQL Server 与 Oracle 驱动尚未实现，这条不能算全绿 |
| 3 | MySQL 上 `DROP COLUMN` 显示「不可回滚且丢数据」红色警示 | 手动 + 集成 ✅ 预案本身说清三件事：`irreversible: true`、`affected_rows` 是非空行数（界面用它说「会丢多少」）、`rollback` 里只有把列加回来的语句；界面把不可逆提示渲染成红色（`.card p.danger` + 红色「不可逆」徽标），`TestIrreversibleWarningIsRed` 钉住类名与配色（改回琥珀色即红）。真机 MySQL 5.7 与 PG 17 都验过：`TestDropColumnIsIrreversibleOnRealEngines`（删列后列真的消失、旁列不受影响、`RollsBackAsCompensation` 与 `DDLTransactional` 一致） |
| 4 | PG 上展示 `CONCURRENTLY` 建议并提示不能用于事务 | 手动 + 集成 ✅ 预案 `notes` 给出 `CREATE INDEX CONCURRENTLY …` 的写法并说明「不能用于事务、要到 SQL 工作区单独跑」，界面把 `plan.notes` 渲染成警示条；`TestCreateIndexPlanGivesConcurrentlyAdvice` 反向锁住「预案里的语句不许自动出现 CONCURRENTLY」（执行门是单事务的，自动生成会让每次执行都失败）。真机 PG 17 核过：事务里建并发索引报 `CREATE INDEX CONCURRENTLY cannot run inside a transaction block`，普通 `CREATE INDEX` 正常 |
| 5 | 加外键前检测孤儿数据并阻断 | 集成 ✅ `TestFKPrecheckOrphansBlocked`、`TestFKPrecheckCleanData`、`TestFKPrecheckCompositeUniqueness`、`TestFKPrecheckEndpoint`、`TestDDLAddForeignKeyOnRealEngines`（MySQL 5.7 + PG 17 真机：孤儿阻断带样本 → 清数据后出预案 → 执行 → 结构里出现外键 → 回滚摘掉） |
| 6 | 加唯一索引前检测重复值并阻断 | 集成 ✅ `TestDDLUniqueIndexBlocksDuplicates`（真机路径在 `TestDDLIndexOnRealEngines`） |
| 7 | 收窄类型前检测越界数据并给出样本 | 集成 ✅ `TestBoundsFor` + `TestDDLAlterColumnTypeOnRealEngines`（MySQL + PG 真机，含拦截与放行两条路径） |
| 8 | 存在视图依赖时 `DROP TABLE` 被阻断 | 集成 ✅ `TestDDLDropTableBlockedByView`、`TestSchemaPlanDropTableBlockedByDependency`、`drivertest` 的 `DependentsReported`（三引擎）、`scripts/accept/deps-drop.ps1`（MySQL 5.7 + PG 17 真机：外键 + 视图双阻断 → 拆掉后出预案 → 执行 → 表消失） |
| 9 | 结构比对生成升级与回退脚本，且脚本可执行 | 集成（双库）✅ `TestCompareSchemas` / `TestCompareIndexes` / `TestCompareForeignKeys` / `TestCompareChecks` / `TestCompareViews` / `TestCompareEndpoint`；真机 `scripts/accept/compare-cross.ps1`（MySQL 5.7 ↔ PG 17 同构表零差异、跨引擎单侧视图分类、MySQL 两库视图零误报） |
| 10 | 对象定义在执行前被他人修改 → 预检失败并阻断 | 集成 ✅ `TestDDLPlanSchemaChangedBlocks`、`TestDDLCreateTableBlocksWhenTableAppeared`、`TestDDLPlanOnRealEngines`（MySQL + PG）；返回 `409 SCHEMA_CHANGED`，前端据此重读结构 |
| 11 | 无长事务时才允许执行高风险 DDL | 集成 ✅ 主体通过：`checkBlockers` 把 `lock_wait` 判为阻断（`ErrLockWait`）、`idle_in_transaction` 只进 `plan.Notes`，`TestSplitBlockers` 覆盖两类分流；**真机验证** `TestHighRiskDDLBlockedByLockWait`（MySQL 5.7：一个事务持行锁、另一事务排队制造真实等锁 → 预案被拒；变异验证：去掉阻断判断即红）。⚠️ **MDL 探测未做**：MySQL 的 `performance_schema.metadata_locks` 需要 8.0，5.7 拿不到，所以「有未提交的元数据锁」这一种情形目前只能靠长事务间接看 |
| 12 | 表单视图与 SQL 视图内容一致（双向） | 单测 ⚠️ **只有单向**：表单 → DDL 由 `BuildDDLPlan` 生成；DDL → 表单（反解）未做，需要完整 DDL 解析器（见 §9 已知限制）。没有自动化用例直接比对两种输入产生同一份语句，此前是人工核对 |

---

## 8. 实现状态（滚动更新）

本节是**契约与代码的一致性台账**：上面是目标契约，这里是「已经做到哪一步」。两者不一致即缺陷——要么补代码，要么把契约降级并说明理由。

### 8.1 已实现

| 能力 | 契约位置 | 实现 | 验证 |
|---|---|---|---|
| 结构详情 | §1.1 | `GET /tables/{name}/schema` 返回列 / 索引 / 外键 / 主键 + DDL 原文 + `capabilities`（前端据此决定摆不摆某个入口：不可用的能力不出现，而不是出现了再点出一个必然失败的错）；DDL 还原失败时仍返回结构并单给 `ddl_error`，不让整页打不开 | `TestSchemaLifecycle` |
| 加列 / 删列 | §1.1（子集） | `POST /schema/plan`，`action` 为 `add_column` / `drop_column`；执行复用 `/changes/{id}/apply` | `TestDDLPlanAddColumn`、`TestDDLPlanDropColumnIsIrreversible` |
| 建表 | §1.1（子集） | `action` 为 `create_table`，按列清单生成 `CREATE TABLE … PRIMARY KEY (…)`；**必须给主键**（无主键的新表在本工具里不可浏览、不可编辑），主键列强制 `NOT NULL`；回滚是 `DROP TABLE` | `TestDDLCreateTable`、`TestSchemaCreateTableAndUndo`、浏览器实测 |
| 表单改动即时生成 DDL 且可见 | §1.2 / 验收 1 | 输入变化 250ms 后调 `?preview=1` 取服务端生成的语句并展示；预览不保管预案 | 浏览器实测 |
| DDL 无法参数化的处理 | §1.2 引申 | 列名走 `QuoteIdent`；类型与默认值走正则白名单（拒绝分号、引号、表达式型默认值） | `TestDDLPlanRejectsBadInput`、`TestSchemaPlanRejectsInjection` |
| 结构变更走同一套 Plan/Apply | §2.1 / ADR-0003 | 预案含语句、回滚、风险（`policy` 分级）、摩擦、有效期；确认后才执行 | `TestSchemaLifecycle` |
| 前置条件：定义未变 | §2.5 / 验收 10 | 预案带表定义哈希，执行前比对，变了返回 `409 SCHEMA_CHANGED` 并让前端重读结构；建表则要求「执行时表仍不存在」（`ExpectTableAbsent`），别人抢先建了就停下 | `TestDDLPlanSchemaChangedBlocks`、`TestDDLCreateTableBlocksWhenTableAppeared`、`TestDDLPlanOnRealEngines`（MySQL + PG） |
| DDL 事务性差异 | §2.3 / 验收 3 | 由 `Capabilities().DDLTransactional` 声明，预案据此设 `rolls_back_as_compensation`；MySQL 上提示「隐式提交，补偿不是撤销」 | `TestDDLPlanOnRealEngines`（MySQL 补偿 / PG 真回滚） |
| 改列类型 | §1.1 / §5.2 | `action` 为 `alter_column_type`；语句文本由 `Dialect.AlterColumnTypeSQL` 渲染（PG `ALTER COLUMN … TYPE`、MySQL `MODIFY COLUMN` 带完整列定义）；**生成预案前做收窄预检**，装不下现存数据即 `ErrNarrowingBlocked` 并给出样本 | `TestBoundsFor`、`TestDDLAlterColumnTypeOnRealEngines`（MySQL + PG）、浏览器实测（含拦截与放行两条路径） |
| 索引管理（建 / 删） | §1.1 / §2.4 / 验收 4、6 | `action` 为 `create_index` / `drop_index`；语句文本由 `Dialect.CreateIndexSQL` / `DropIndexSQL` 渲染（MySQL 的 `DROP INDEX` 要带 `ON <table>`，PG/SQLite 不能带）；建唯一索引前先查重复值，命中即 `ErrDuplicateRows` 并给样本；主键索引拒绝删除；删索引不动数据，回滚按原定义重建。**建索引预案附带在线变更建议**（`Dialect.IndexBuildAdvice`）：PG 提示 `CREATE INDEX CONCURRENTLY` 并说明它不能在事务里跑、需要到 SQL 工作区单跑；MySQL 提示 `ALGORITHM=INPLACE, LOCK=NONE`；SQLite 不给（没有这个概念） | `TestDDLCreateAndDropIndex`、`TestDDLUniqueIndexBlocksDuplicates`、`TestDDLIndexOnRealEngines`（MySQL + PG，直接验证 `DROP INDEX` 写法与建议非空）、`TestIndexBuildAdvice`、浏览器实测 |
| 结构比对（v1） | §4 / 验收 9 | `POST /compare`：两个已打开连接逐表比对，输出 `only_left` / `only_right` / `changed` / `need_review` 与逐项原因（列：中立类型、原始类型、可空、默认值、注释、主键与唯一标记；索引：列序列+唯一性+谓词；外键：本表列 → 引用表+引用列，动作子句参与比较但不参与键；检查约束：归一化表达式）。原始类型文本先做同义归一（`character varying(n)`≡`varchar(n)`、`integer`≡`int`、剥掉 MySQL 整数显示宽度）。索引增删、视图增删两个方向都给脚本（删它们不丢数据）；外键在左侧引擎支持 `AddForeignKey` 时给加/删约束脚本（否则进 `need_review` 说明要重建表）；删表/删列/改类型与检查约束仍进 `need_review`。**视图只比查询体**（`ALGORITHM` / `DEFINER` / `SQL SECURITY` 是部署属性，不进比对），查不出定义时不下「相同」结论。每条差异都带两侧结构原文 `left_ddl` / `right_ddl` | `TestCompareSchemas`、`TestCompareIndexes`、`TestCompareIndexNameOnlyDifference`、`TestCompareForeignKeys`、`TestCompareChecks`、`TestCompareForeignKeysEndToEnd`、`TestCompareViews`、`TestViewQueryBody`、`TestNormalizeViewBody`、`TestViewReasons`、`TestNormalizeTypeText`、`TestCompareEndpoint`、浏览器实测；`TestCompareIndexes` 与视图提取（`TestViewQueryBody`）均含变异验证；真机 `scripts/accept/compare-cross.ps1` 三段：MySQL 5.7 ↔ PostgreSQL 17 同构表**零差异**、跨引擎单侧视图分类与原文、MySQL 两库同构视图**零误报**（`DEFINER` / `ALGORITHM` 头部不计入） |
| 加外键 | §1.1 / 验收 5 | `action` 为 `add_foreign_key`；生成预案前**先跑与预检端点同一套检查**（孤儿数据 + 父表列唯一性），不通过即 `ErrForeignKeyBlocked` → 409 `FK_PRECHECK_BLOCKED` 并带样本；动作子句走白名单（CASCADE / RESTRICT / NO ACTION / SET NULL），`SET NULL` 撞上 NOT NULL 引用列时提前拒绝；名字为空按 `fk_<表>_<列>` 生成（回滚要靠它摘约束，不能是随机名）；回滚是 `ALTER TABLE … DROP` 约束，不动数据 | `TestValidateFKShape`、`TestDefaultFKName`、`TestDDLAddForeignKeyNeedsCapability`、`TestSchemaPlanAddForeignKeyNeedsCapability`、`TestDDLAddForeignKeyOnRealEngines`（MySQL 5.7 + PG 17：孤儿阻断带样本 → 清数据后出预案 → 执行 → 结构里出现外键 → 回滚摘掉） |
| 手写 DDL（单向） | §1.2 / 验收 12 | `action` 为 `raw` + `sql`：切分 → 必须是恰好一条 → 必须是 DDL 类 → 生成预案走同一个 apply。**不做语义预检**（不查收窄/孤儿/依赖），**不生成回滚语句**，预案里两条都写明并标不可逆；指明了存在的表时仍带定义哈希做竞态保护。多条语句、DML、空语句各有明确拒绝理由 | `TestRawDDLPlan`（五条断言：正常 ALTER、多语句拒绝、DML 拒绝并指路变更集、空语句拒绝、执行后列真的加上） |
| 删表 + 依赖阻断 | §5.1 / 验收 8 | `action` 为 `drop_table`：生成预案前先查依赖，被视图或入向外键引用即 `409 DEPENDENCY_BLOCKED` 并列出引用方（近似结果带「可能误报」标注）；删表必然不可逆，回滚语句只是把空表建回来，预案里明说「数据不会回来」 | `TestDDLDropTableBlockedByView`、`TestDDLDropColumnBlockedByForeignKey`、`TestSchemaPlanDropTableBlockedByDependency`、`drivertest` 的 `DependentsReported`（三引擎）、`scripts/accept/deps-drop.ps1`（MySQL 5.7 + PG 17 真机：外键 + 视图双阻断 → 拆掉后出预案 → 执行 → 表消失） |
| 删列的外键阻断 | §5.1 | 删列前查入向外键；引用该列即阻断并点名引用方（外键依赖是精确的，不标近似）；视图依赖只写进 `notes` 提示自查 | `TestDDLDropColumnBlockedByForeignKey` |
| 预执行检查：长事务与锁等待 | §2.5 / 验收 11 | `Metadata.Blockers`：MySQL 走 `innodb_trx`（超 60 秒的事务）+ `innodb_lock_waits`；PG 走 `pg_stat_activity`（`idle in transaction` 与 `wait_event_type='Lock'`）；SQLite 恒为空（单写者模型）。**锁等待阻断**（`409 LOCK_WAIT_BLOCKED`），**长事务只写进 `notes`**（最多 3 条）——忙碌的库上永远有人开着事务，用它阻断等于关掉整个设计器 | `drivertest` 的 `BlockersReported`（自己开一个事务再从另一条连接查，断言必须报出来；SQLite 明确 skip）、`TestSchemaPlan*` 系列不回归 |
| 预执行检查：元数据锁（MDL） | §2.5 / 验收 11 | 未做：MySQL 5.7 没有 `performance_schema.metadata_locks`，做不了就不假装能做 | 待有 MySQL 8 实测环境后再补 |
| 新建触发器 | §1.1 / §6 | `action` 为 `create_trigger`（`trigger:{name,timing,event,body}`，目标表由 `table` 给）：MySQL 渲染 `CREATE TRIGGER … FOR EACH ROW <体>`，SQLite 渲染 `… BEGIN <体>; END`，回滚 `DROP TRIGGER`。**体只支持单条语句**（含分号即拒）：MySQL 的多语句体要靠 `DELIMITER`，那是客户端语法，我们走驱动。时机/事件走白名单（`BEFORE`/`AFTER`/`INSTEAD OF`、`INSERT`/`UPDATE`/`DELETE`）。PG 声明 `AddTrigger: false`——它要先建函数，生成一条 `CREATE TRIGGER` 只会得到必然失败的语句 | `TestDDLCreateTriggerValidation`（6 类非法输入）、`TestDDLCreateTriggerWorksOnSQLite`（`AFTER INSERT` 真的把 n 翻倍 → 回滚后不再翻倍；顺带验证 `BEGIN … END` 里的分号没被切分器切断）、`TestDDLCreateTriggerOnRealEngines`（MySQL 5.7：`BEFORE INSERT SET NEW.v = UPPER(NEW.v)`，插 `abc` 读回 `ABC`；PG：被拒并说明要先 `CREATE FUNCTION`）；界面按能力位摆 |
| 新建物化视图 | §1.1 / §6 | `action` 为 `create_matview`（复用 `view:{name,query}`）：`CREATE MATERIALIZED VIEW … AS …`，回滚 `DROP MATERIALIZED VIEW`。与普通视图共用视图体校验；**预案里必须写明它会存结果、要 `REFRESH` 才更新**——用户看到「视图」两个字默认以为实时。只在 `MaterializedViews` 能力为真的引擎上出现（PG），界面上是「新建视图」里的一个勾选项而不是第四个按钮 | `TestDDLCreateMatViewOnRealPG`（PG 17：建成 → 源表插新行后视图**仍是 1 行**（证明真的物化）→ `REFRESH` 后变 2 行 → 回滚后不存在）、`TestDDLCreateMatViewRejectedEngines`（SQLite/MySQL 拒绝并说明没有这个概念）；变异验证：把 `CREATE MATERIALIZED VIEW` 改成 `CREATE VIEW` 即红 |
| 新建序列 | §1.1 / §6 | `action` 为 `create_sequence`（`sequence:{name,start,increment,min,max,cycle}`，数字用指针，留空即交给引擎默认）：`CREATE SEQUENCE …`，回滚 `DROP SEQUENCE`。参数先校验再判引擎能力：步长 0、最小值大于最大值、起始值越界都在我们这一侧拒掉。**只有声明了 `Sequences` 的引擎才有这个按钮**（PG；MySQL 用 AUTO_INCREMENT、SQLite 用 AUTOINCREMENT） | `TestDDLCreateSequenceRejectedEngines`、`TestDDLCreateSequenceRejectsBadInput`（4 类非法参数）、`TestDDLCreateSequenceOnRealPG`（PG 17：带 `START WITH 100`/`INCREMENT BY 5` → `nextval` 可用 → 回滚后报不存在） |
| 新建枚举类型 | §1.1 / §6 | `action` 为 `create_type`（`type:{name,values}`，v1 只做枚举）：`CREATE TYPE … AS ENUM (…)`，回滚 `DROP TYPE`。**不加 `IF EXISTS` 也不带 `CASCADE`**——删一个还被列用着的类型必须失败，悄悄级联会把依赖它的列一起删掉。值先校验再判能力：空值、重复值、超过 63 字节（PostgreSQL 的标签存在 `pg_enum.enumlabel`，那是 `name` 类型）、含控制字符都在我们这一侧拒掉，并指出是哪个值；**顺序照填写的顺序进语句**，因为枚举的比较大小按声明顺序而不是字母序。同名类型直接拒绝，并提示要加值请用 `ALTER TYPE … ADD VALUE`。预案说明写清两件事：类型本身不存数据，但一旦有列在用、删它就等于让那些列读不出来；值只能往后追加，删或重排会让已有数据读出另一个值。**对象树同步接上**：`/tables` 在 `UserTypes` 为真时返回类型节点，点开走 `/tables/{name}/schema?kind=type` 取定义（枚举按 `enumsortorder` 拼回 `CREATE TYPE`；域连 `NOT NULL` / `DEFAULT` / `CHECK` 一起还原——只写 `AS <基类型>` 会建出一个少了一半约束的东西） | `TestDDLCreateTypeRejectedEngines`、`TestDDLCreateTypeRejectsBadInput`（6 类非法输入）、`TestDDLCreateTypeOnRealPG`（PG 17：带单引号的值建成 → 建表写入 → `state > 'pending'` 命中两行证明顺序是声明序 → 定义能还原 → 还被列用着时 `DROP TYPE` 失败 → 删表后回滚，类型消失）、`TestSchemaPlanAcceptsType`（HTTP 可达：**这条一写好就抓到了字段转交里漏掉 `Type` 的问题**——请求结构有字段、处理器逐字段转交时没带上，界面点一次才会炸）、`TestUICreateTypeIsWired`（界面接线：默认 hidden、按 `user_types` 显示、`action` 与字段名与服务层一致） |
| 例程列表与定义 | §1.1 / §6 | `Objects(KindRoutine)`：MySQL 走 `information_schema.ROUTINES`，PostgreSQL 走 `pg_proc` 的 `prokind IN ('f','p')` 并排除聚合 / 窗口函数 / 扩展成员（`pg_depend.deptype='e'`）；SQLite 没有例程，能力位为 `false`。对象树把它们当「有定义没数据」的对象摆（与触发器 / 序列 / 类型同一条渲染分支），点击走 `/tables/{name}/schema?kind=routine`。定义还原：MySQL 先查 `ROUTINE_TYPE` 再 `SHOW CREATE PROCEDURE` / `SHOW CREATE FUNCTION`（两者结果列名不同，猜错会得到一句「对象不存在」），PostgreSQL 用 `pg_get_functiondef`。**PG 的重载**：一个名字下可以有多条，列表按名字去重、定义只显示第一条并在开头注明共几条——把几条拼在一起，读的人会以为那是**一条**可回放的语句 | `drivertest` 的 `RoutineListing`（MySQL 5.7 + PG 17 真机：建一个带 `BEGIN … END` 体的过程 → 列出来 → 取定义 → 删掉后**立刻**消失（挡住缓存）→ 用取到的定义**回放**成功 → 又能列出来）、`TestRoutineCapabilityIsHonest`（SQLite `false` / MySQL `true` / PG `true`，写错即红）；变异验证：把 PG 的提前返回改回只带 `triggers`，`RoutineListing` 立即红 |
| 新建视图 | §1.1 / §6 | `action` 为 `create_view`（`view:{name,query}`）：`CREATE VIEW <名> AS <query>`，回滚 `DROP VIEW`。**不加 `OR REPLACE`**——SQLite 不支持它，且「悄悄替换掉一个已有视图」等于改掉所有依赖方看到的列；同名存在时直接拒绝。视图体按「是不是一条查询」卡：必须 SELECT/WITH 开头、禁分号、禁控制字符、≤4000 字符。界面上「新建视图」进同一套预案 → 确认 → 执行，建完刷新对象树 | `TestDDLCreateView`（SQLite：建 → 查得到 → 回滚 → 没了）、`TestDDLCreateViewOnRealEngines`（MySQL 5.7 + PG 17 同一套流程）、`TestDDLCreateViewRejectsBadInput`（空定义 / 非查询 / 带分号 / 名字非法 / 同名已存在）；变异验证：跳过同名检查即红 |
| 加检查约束 | §1.1 / §5.2 | `action` 为 `add_check`（`check:{name,expression}`）：PG 渲染 `ALTER TABLE … ADD CONSTRAINT … CHECK (…)`，回滚是 `DROP CONSTRAINT`。**只给真会强制的引擎开**（`Capabilities().AddCheck`）：MySQL 5.7 会解析 CHECK 却静默忽略（真机验证：`ADD CONSTRAINT … CHECK (age >= 0)` 成功、插 `age=-100` 也成功），8.0.16 起才强制；SQLite 只能在建表时写。加之前先数一遍 `NOT (expr)` 的行数，有值即 `ErrCheckViolated` 并报行数。表达式按「不像注入」卡：禁分号（会把一条语句变两条）、禁注释符、禁控制字符、长度上限 1000 | `TestDDLAddCheckOnRealEngines`（PG 17：违规行阻断带行数 → 修数据 → 执行 → 插违规行被拒 → 回滚后再插成功；MySQL 5.7：先证明它忽略 CHECK，再断言我们拒绝并写清 8.0.16 边界）、`TestDDLAddCheckRejectsBadInput`（6 条非法输入）、`TestDDLAddCheckRejectedEngines`；界面按能力位决定摆不摆。**内置浏览器实测**（PG 17）：结构页在 `id`（非空）上显示「改可空」、在 `v`/`n`（可空）上显示「改非空」，检查约束区一并渲染；点「加约束」空表单给出「约束名与表达式都要填」。⚠️ 「改非空」的成功路径（出预案 → 确认 → 执行）仍未点过，由后端真机用例覆盖 |
| 改可空 | §1.1 / §5.2 | `action` 为 `set_nullable`：PG 直接 `ALTER COLUMN … SET/DROP NOT NULL`；MySQL 走 `MODIFY COLUMN`（必须带完整列定义，否则会顺手丢默认值）；SQLite 声明 `AlterColumnNullability: false`，直接拒绝。**改非空前先数了一遍 NULL**，有值即 `ErrNullableBlocked` 并报「N 行为 NULL」——交给引擎报错拿不到行数。改成与现状相同的状态也拒绝（预案说改了什么其实没改） | `TestDDLSetNullableOnRealEngines`（MySQL + PG：NULL 阻断带行数 → 补值 → 执行 → 回滚变回可空）、`TestDDLSetNullableAndDefaultUnsupportedOnSQLite`；变异验证：去掉 NULL 预检即红。界面按钮已内置浏览器实测到「按状态显示哪个按钮」这一层（见上一条） |
| 改默认值 | §1.1 / §5.2 | `action` 为 `set_default`：`ALTER COLUMN … SET/DROP DEFAULT`；值必须过 `defaultPattern`（数字 / 单引号字符串 / TRUE / FALSE / CURRENT_TIMESTAMP），表达式一律拒——它是一条任意 SQL。**当前默认值本身是表达式时也拒**：那意味着给不出等价回滚。预案注明「只影响之后的写入，不回填已有行」 | `TestDDLSetDefaultOnRealEngines`（MySQL + PG：表达式被拒 → SET DEFAULT → DROP DEFAULT）、`TestDDLSetNullableAndDefaultUnsupportedOnSQLite` |
| 外键预检（只读） | §5.1 / §5.2 / 验收 5 | `POST /connections/{id}/fk/precheck`：检出子表里父表找不到对应行的数据（任一子列为 NULL 不计），判断父表参与列是否唯一（主键或**列集合完全一致**的唯一索引），orphans>0 即 `blocked` 并给最多 5 条样本 | `TestFKPrecheckCleanData`、`TestFKPrecheckOrphansBlocked`、`TestFKPrecheckCompositeUniqueness`、`TestFKPrecheckEndpoint` |
| 删列的影响面 | §5.2 | 展示该列**非空行数**（`COUNT(*) WHERE col IS NOT NULL`），并标 `不可逆`；NULL 行不计入 | 浏览器实测（3 行有值 → 显示「影响 3 行」） |
| 加列可撤销 | §2.6 | 撤销预案 = `DROP COLUMN`，复用撤销通道；删列因数据丢失不生成撤销 | 浏览器实测（加列后撤销，结构与 DDL 复原） |

### 8.2 尚未实现（对应验收项也未通过）

| 缺口 | 契约位置 | 影响 | 处置 |
|---|---|---|---|
| 例程设计器 | §1.1、§6 | 缺失（**用户类型（枚举）已补上**：能列、能建、能看定义，见 §8.1「新建枚举类型」；**普通视图**（三引擎）、**物化视图**与**序列**（PG）、**触发器**（MySQL / SQLite）都已可建，见下方四条。**例程**的列表与定义查看已做（见 §8.1「例程列表与定义」），新建仍只能走「手写 DDL」，而那条路现在通了：切分器认得 `CREATE PROCEDURE / FUNCTION … BEGIN … END`，不写 `DELIMITER` 也能整条发过去。**PG 的触发器**需先 `CREATE FUNCTION`，v1 不做，服务端会明确拒绝并说明 | 例程设计器的待实现部分只剩「表单 + 预案 + 回滚（DROP PROCEDURE）」——语句本身已经能发 |
| 引擎特有项（`EngineExtra`）表单 | §1.3 | 未渲染，仅能看 DDL 原文 | 待实现；依赖各引擎补充元数据 |
| 磁盘余量探测 | §2.5 | 未做：DDL 前不检查可用空间，大表重建可能把磁盘写满 | 待实现；需要各引擎的库体积与所在卷余量 |
| 例程依赖检查 | §5.1 | 只查视图与外键；存储过程/函数体里引用目标对象查不出来 | 待实现；文本搜索例程体误报率太高，加之前要先想清楚怎么标注 |
| 结构比对的完整形态 | §4 / 验收 9 | 已比表、列、索引、外键、检查约束、视图定义；回退脚本覆盖索引、外键与视图；删表/删列/改类型与检查约束仍不生成（会丢数据，有意推给人）；物化视图与例程不参与比对（`§4.3`） | 物化视图与例程待实现；前者要区分 `DROP MATERIALIZED VIEW`，后者在三种引擎里几乎无可比性 |
| 手写 SQL 模式（表单 ↔ SQL 双向同步） | §1.2 / 验收 12 | 已做**单向**：写一条 DDL → 校验 → 走同一个 Plan/Apply。**没做**「SQL 反解回表单」——那需要完整的 DDL 解析器 | 双向同步待实现；单向已覆盖「我已经有语句，只想让它经过这道门」这个真实需求 |

**界面能力位实测（2026-11-05，内置浏览器）**：同一份前端代码在两种连接上表现符合预期——

| 连接 | 结构页出现的区块 | 顶栏按钮 | 新建视图对话框 |
|---|---|---|---|
| SQLite | 列 / 索引 / **触发器**；**没有**检查约束 | **没有**「新建序列」 | **没有**「物化视图」勾选 |
| PostgreSQL | 列 / 索引 / **检查约束**；**没有**触发器 | **有**「新建序列」 | **有**「物化视图」勾选，且带「要 REFRESH 才更新」的说明 |

这条是补验：新增的四块 UI（触发器、检查约束、新建序列、物化视图勾选）此前只有源码门禁与后端真机用例，没在浏览器里点过；上两轮各有一次「服务层绿、界面点了才炸」，所以这次一次性把两个方向都点开看。

**「新建类型」端到端实测（2026-11-05，内置浏览器 + PG 17 真机）**：同一份前端，SQLite 上不摆这个按钮；PostgreSQL 上按钮出现，点击后按「类型名 + 一行一个值」填 `ui_state` / `pending, paid, refunded`，预案弹窗给出 `CREATE TYPE "ui_state" AS ENUM ('pending', 'paid', 'refunded')`（值与顺序与填写一致）、风险 R3 / 打扰 F1，并渲染两条 `notes`（类型不存数据、值只能往后追加）；点「执行」后左侧对象树立刻出现 `ui_state · 类型`，点开显示还原出的 `CREATE TYPE "public"."ui_state" AS ENUM ('pending', 'paid', 'refunded');`。这一步同时验了两件事：定义还原走的是 `pg_enum` 的 `enumsortorder`（顺序没被按字母序重排），以及对象树确实把类型当成「有定义没数据」的对象来摆（与触发器/序列同一条渲染分支）。测试结束用 `DROP TYPE` 清掉，未在测试库里留对象。

### 8.3 验收清单状态

已过：1（预览）、3（MySQL 警示）、4（建索引时按引擎给出在线变更建议，并说明 `CONCURRENTLY` 不能在事务里跑）、5（加外键前检测孤儿并阻断，含样本；MySQL/PG 真机跑通「阻断 → 出预案 → 执行 → 结构出现 → 回滚摘掉」）、6（唯一索引前查重复值并阻断，含样本）、7（收窄预检 + 样本）、8（删表/删列前查依赖并阻断，引用方点名，MySQL/PG 真机验证）、10（定义变更阻断）、11（长事务与锁等待探测：锁等待阻断、长事务只提示，drivertest 用「自己开着事务」验证）。
未过：2（MSSQL/Oracle 未实现，无法 5 引擎回放）、11 的 MDL 部分（MySQL 5.7 无 metadata_locks）；12（手写 SQL 模式）部分过：单向（写 DDL → 校验 → 执行）已可用，SQL 反解回表单未做；9（结构比对）部分过：表/列/索引/外键/检查约束都已比，升级与回退脚本覆盖索引与外键，但删表/删列/改类型与检查约束不生成回退脚本，视图定义与例程尚未比对。

