# ADR-0005 · 跨引擎差异用能力声明表达，禁止按引擎名分支

- 状态：**已采纳**
- 日期：2026-11-03
- 影响面：`model.Capabilities`、各 `driver/*`、`service/ddl`、`service/change`

---

## 背景

实现结构设计器时连续撞到两类差异：

1. **能力有无**：SQLite 根本不支持「修改列类型」（只能重建表）。PostgreSQL 与 MySQL 支持，但语法不同（`ALTER COLUMN … TYPE` vs `MODIFY COLUMN`）。
2. **语法形态**：`DROP INDEX` 在 MySQL 上必须写 `DROP INDEX idx ON tbl`，PostgreSQL / SQLite 上不能带 `ON tbl`。

最容易的写法是 `switch engine { case model.EngineMySQL: … }`。这条路的问题是它会扩散：

- 每加一个引擎都要回头改所有 `switch`；漏一处就是「新引擎能连上但某个操作静默走错分支」。
- `switch` 没有默认值语义。未知引擎落到哪个分支全看代码顺序，而 `unknown` 是本项目里最危险的取值（见 ADR-0004）。
- 测试无法枚举「所有引擎在某个能力上的取值」，只能逐个写用例。

`internal/ci` 的状态里已经有一条「新增引擎必须通过一致性套件」的门禁，但那是行为门禁，管不住实现方式。

---

## 决策

### 1. 能力有无 → 写进 `Capabilities`

新增字段的判据：**「这个操作在某个引擎上是否根本不成立」**。

已经这么用的例子：`DDLTransactional`（DDL 是否参与事务）、`KeysetPagination`、`MaterializedViews`。

本次新增：

```go
// AlterColumnType 表示引擎支持原地修改列类型。
// SQLite 为 false（只能重建表），PostgreSQL / MySQL 为 true。
AlterColumnType bool `json:"alter_column_type"`
```

上层只读这个字段。`false` 时给出明确拒绝理由，而不是生成一条必然失败的语句。

### 2. 语法形态 → 归 `internal/driver/dialect`

判据：**「同样的语义，不同引擎要写的字面文本不同」**。

这类差异不该上升到「能力」，因为操作本身成立；也不该散落到上层，因为「哪种字面文本」是方言的本职。`dialect` 包因此被允许在内部按引擎给出不同渲染，但**只对外暴露与引擎无关的方法**。

例如改列类型：PostgreSQL 用 `ALTER TABLE … ALTER COLUMN … TYPE`，MySQL 用 `MODIFY COLUMN`（且必须给完整列定义，否则 `NOT NULL` 与默认值会被一起丢掉），SQLite 不支持。对外是一个方法：

```go
// AlterColumnTypeSQL 返回把列改成新类型所需的语句（可能不止一条）。
AlterColumnTypeSQL(table, column, typeName, defaultText string, nullable bool) []string
```

### 3. 引擎名只能出现在两个包

除以下两处，代码里不得出现 `switch`/`if` 判断具体引擎：

| 允许位置 | 理由 |
|---|---|
| `internal/driver/dialect` | 方言的本职：标识符引用、词法开关、以及**引擎专属的语句文本** |
| 各驱动的构造函数（`sqlite.New()` 等） | 装配点，由调用方按配置选择 |

`service` / `api` / `agent` 一律只读 `Capabilities()` 与 `Dialect` 的方法，不认引擎名。

判定方式：`grep -rn 'EngineMySQL\|EnginePostgreSQL\|EngineSQLite' internal/service internal/api` 的命中数应当为 0；新引擎接入时该命令的输出不应变化。

### 4. 新增字段的门槛

`Capabilities` 是跨模块接口，加字段要满足三条：

1. **语义单一**：一个字段只回答一个问题，不用「模式字符串」把多个差异打包。
2. **默认值安全**：零值必须是「保守」的一侧。`false` 表示「不支持/需要额外处理」，让新引擎在未声明时行为收敛而不是放开。
3. **有一致性用例**：`drivertest` 里至少一条用例断言该字段与实际行为一致（例：`AlterColumnType=false` 的引擎上，改列类型必须被拒绝而不是报语法错）。

---

## 后果

**正面**

- 新增引擎只改「装配 + 声明 + 能力实现」，不回头翻上层逻辑。
- 上层可以对能力做穷举断言，测试覆盖从「逐引擎」变成「按能力」。
- 未知引擎默认走保守分支，符合「未声明即最严」的既有原则。

**成本**

- 能力字段会变多，需要克制：只有真的出现分歧才加，不为「将来可能需要」预留。
- 一个操作同时受「能力」和「语法」影响时（如索引），要判断差异属于哪一类。判据已写在第 1、2 条，但仍有判断成本。

---

## 替代方案

| 方案 | 否决理由 |
|---|---|
| 上层 `switch engine` | 见背景：扩散、无默认值语义、无法枚举测试 |
| 每个驱动各自实现一份 DDL 生成 | 上层无法做统一的 Plan/Apply 与风险分级；重复实现必然分叉 |
| 只支持三引擎共有的操作 | 会把「SQLite 不能改列类型」变成「所有引擎都不能改列类型」，牺牲的是真实能力 |
