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

执行前逐条校验，任一失败则阻断：

- 对象仍存在且定义未变（比对 `DDL` 的哈希）→ 防止「我看着旧的改，别人已经改了」。
- 无长事务（超过阈值）→ MySQL/PG 会因此阻塞。
- 无活跃的 DDL（`metadata_lock` / `MDL`）。
- 磁盘空间充足（估算体积 × 2.5）。
- 外键相关表无孤儿数据（加外键前）。

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

### 4.2 比对规则

- 类型比对走中立类型 + 原始类型双轨：中立类型不同必报差异；中立相同但原始不同（如 `varchar(50)` vs `varchar(100)`）也报差异并标注。
- 默认值比对做归一化（去外层括号、统一 `now()` 与 `CURRENT_TIMESTAMP` 视为等价需**引擎内**判断，跨引擎不视为等价）。
- 索引比对按「列序列 + 唯一性 + 条件」为键，不按索引名（名字不同不一定是差异）。
- 跨引擎比对的类型差异一律标为「需人工决策」，不自动映射。

### 4.3 边界

- 不做数据比对（那是迁移模块的事）。
- 不自动执行比对结果，必须经过 Change Plan。
- 大 schema（>2000 对象）分批比对并显示进度。

---

## 5. 依赖与安全

### 5.1 依赖检查

重命名/删除对象前，查询引用关系：

| 依赖类型 | 来源 |
|---|---|
| 视图依赖 | `information_schema.view_table_usage` / `pg_depend` / `sys.sql_expression_dependencies` / `ALL_DEPENDENCIES` |
| 外键依赖 | 元数据外键清单 |
| 例程依赖 | 引擎文本搜索（不可靠，需标注） |
| 应用代码依赖 | 不覆盖（超出范围） |

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
| 1 | 表设计器改动实时生成 DDL 且可见 | 手动 |
| 2 | 5 种关系库的 DDL 还原可空库回放并结构等价 | `drivertest` |
| 3 | MySQL 上 `DROP COLUMN` 显示「不可回滚且丢数据」红色警示 | 手动 |
| 4 | PG 上展示 `CONCURRENTLY` 建议并提示不能用于事务 | 手动 |
| 5 | 加外键前检测孤儿数据并阻断 | 集成 |
| 6 | 加唯一索引前检测重复值并阻断 | 集成 |
| 7 | 收窄类型前检测越界数据并给出样本 | 集成 |
| 8 | 存在视图依赖时 `DROP TABLE` 被阻断 | 集成 |
| 9 | 结构比对生成升级与回退脚本，且脚本可执行 | 集成（双库） |
| 10 | 对象定义在执行前被他人修改 → 预检失败并阻断 | 集成 |
| 11 | 无长事务时才允许执行高风险 DDL | 集成 |
| 12 | 表单视图与 SQL 视图内容一致（双向） | 单测 |

---

## 8. 实现状态（滚动更新）

本节是**契约与代码的一致性台账**：上面是目标契约，这里是「已经做到哪一步」。两者不一致即缺陷——要么补代码，要么把契约降级并说明理由。

### 8.1 已实现

| 能力 | 契约位置 | 实现 | 验证 |
|---|---|---|---|
| 结构详情 | §1.1 | `GET /tables/{name}/schema` 返回列 / 索引 / 外键 / 主键 + DDL 原文；DDL 还原失败时仍返回结构并单给 `ddl_error`，不让整页打不开 | `TestSchemaLifecycle` |
| 加列 / 删列 | §1.1（子集） | `POST /schema/plan`，`action` 为 `add_column` / `drop_column`；执行复用 `/changes/{id}/apply` | `TestDDLPlanAddColumn`、`TestDDLPlanDropColumnIsIrreversible` |
| 建表 | §1.1（子集） | `action` 为 `create_table`，按列清单生成 `CREATE TABLE … PRIMARY KEY (…)`；**必须给主键**（无主键的新表在本工具里不可浏览、不可编辑），主键列强制 `NOT NULL`；回滚是 `DROP TABLE` | `TestDDLCreateTable`、`TestSchemaCreateTableAndUndo`、浏览器实测 |
| 表单改动即时生成 DDL 且可见 | §1.2 / 验收 1 | 输入变化 250ms 后调 `?preview=1` 取服务端生成的语句并展示；预览不保管预案 | 浏览器实测 |
| DDL 无法参数化的处理 | §1.2 引申 | 列名走 `QuoteIdent`；类型与默认值走正则白名单（拒绝分号、引号、表达式型默认值） | `TestDDLPlanRejectsBadInput`、`TestSchemaPlanRejectsInjection` |
| 结构变更走同一套 Plan/Apply | §2.1 / ADR-0003 | 预案含语句、回滚、风险（`policy` 分级）、摩擦、有效期；确认后才执行 | `TestSchemaLifecycle` |
| 前置条件：定义未变 | §2.5 / 验收 10 | 预案带表定义哈希，执行前比对，变了返回 `409 SCHEMA_CHANGED` 并让前端重读结构；建表则要求「执行时表仍不存在」（`ExpectTableAbsent`），别人抢先建了就停下 | `TestDDLPlanSchemaChangedBlocks`、`TestDDLCreateTableBlocksWhenTableAppeared`、`TestDDLPlanOnRealEngines`（MySQL + PG） |
| DDL 事务性差异 | §2.3 / 验收 3 | 由 `Capabilities().DDLTransactional` 声明，预案据此设 `rolls_back_as_compensation`；MySQL 上提示「隐式提交，补偿不是撤销」 | `TestDDLPlanOnRealEngines`（MySQL 补偿 / PG 真回滚） |
| 改列类型 | §1.1 / §5.2 | `action` 为 `alter_column_type`；语句文本由 `Dialect.AlterColumnTypeSQL` 渲染（PG `ALTER COLUMN … TYPE`、MySQL `MODIFY COLUMN` 带完整列定义）；**生成预案前做收窄预检**，装不下现存数据即 `ErrNarrowingBlocked` 并给出样本 | `TestBoundsFor`、`TestDDLAlterColumnTypeOnRealEngines`（MySQL + PG）、浏览器实测（含拦截与放行两条路径） |
| 索引管理（建 / 删） | §1.1 / 验收 6 | `action` 为 `create_index` / `drop_index`；语句文本由 `Dialect.CreateIndexSQL` / `DropIndexSQL` 渲染（MySQL 的 `DROP INDEX` 要带 `ON <table>`，PG/SQLite 不能带）；建唯一索引前先查重复值，命中即 `ErrDuplicateRows` 并给样本；主键索引拒绝删除；删索引不动数据，回滚按原定义重建 | `TestDDLCreateAndDropIndex`、`TestDDLUniqueIndexBlocksDuplicates`、`TestDDLIndexOnRealEngines`（MySQL + PG，直接验证 `DROP INDEX` 写法）、浏览器实测 |
| 结构比对（v1） | §4 / 验收 9 | ``POST /compare``：两个已打开连接逐表比对，输出 ``only_left`` / ``only_right`` / ``changed`` / ``need_review`` 与逐列原因（中立类型、原始类型、可空、默认值、注释）；升级脚本只生成**增量**（建缺失的表、加缺失的列），删除与改类型一律进 ``need_review`` | ``TestCompareSchemas``、``TestCompareEndpoint``、浏览器实测（3 张差异表 + 升级脚本 + 3 条待决策） |
| 删列的影响面 | §5.2 | 展示该列**非空行数**（`COUNT(*) WHERE col IS NOT NULL`），并标 `不可逆`；NULL 行不计入 | 浏览器实测（3 行有值 → 显示「影响 3 行」） |
| 加列可撤销 | §2.6 | 撤销预案 = `DROP COLUMN`，复用撤销通道；删列因数据丢失不生成撤销 | 浏览器实测（加列后撤销，结构与 DDL 复原） |

### 8.2 尚未实现（对应验收项也未通过）

| 缺口 | 契约位置 | 影响 | 处置 |
|---|---|---|---|
| 改默认值 / 改可空 | §1.1 | 改列类型时会把原有的可空性与默认值一起保留，但还不能单独修改它们 | 待实现；改默认值涉及表达式白名单，改可空涉及 NOT NULL 预检 |
| 外键 / 检查约束管理 | §1.1 | 不能加外键、编辑检查约束；索引已支持建删（含唯一索引重复值预检），但**外键的孤儿数据预检**（§5.2 / 验收 5）仍未做 | 待实现；外键需要先有孤儿数据预检 |
| 视图 / 物化视图 / 例程 / 触发器 / 序列 / 用户类型设计器 | §1.1、§6 | 全部缺失 | 待实现 |
| 引擎特有项（`EngineExtra`）表单 | §1.3 | 未渲染，仅能看 DDL 原文 | 待实现；依赖各引擎补充元数据 |
| 在线变更建议（`CONCURRENTLY`、`ALGORITHM=INPLACE` 等） | §2.4 / 验收 4 | 无建议与「一键插入编辑器」 | 待实现 |
| 预执行检查（长事务 / MDL / 磁盘 / 孤儿数据） | §2.5 / 验收 5、11 | 只做了「定义哈希」一项；其余未探测 | 待实现 |
| 类型收窄预检与样本行 | §5.2 / 验收 7 | 改类型未做，故也没有越界检查 | 待实现 |
| 依赖检查（删对象的引用关系） | §5.1 / 验收 8 | 删表/删列前不查视图与外键依赖 | 待实现 |
| 结构比对的完整形态 | §4 / 验收 9 | v1 只比对表与列，未比索引/外键/检查约束/视图定义；升级脚本只有增量，回退脚本为空 | 待实现；索引比对要按「列序列+唯一性+条件」为键，不能按名字 |
| 手写 SQL 模式（表单与 SQL 双向同步） | §1.2 / 验收 12 | 只有只读预览，没有可编辑的手写模式 | 待实现；手写模式需要表达式与 DDL 校验 |

### 8.3 验收清单状态

已过：1（预览）、3（MySQL 警示）、6（唯一索引前查重复值并阻断，含样本）、7（收窄预检 + 样本）、10（定义变更阻断）。
未过：2（MSSQL/Oracle 未实现，无法 5 引擎回放）、4（在线变更建议）、5（外键孤儿数据预检）、8（依赖检查）、11（长事务探测）、12（手写 SQL 模式）；9（结构比对）部分过：差异分类与升级脚本已可用，回退脚本尚未生成。

