# ADR-0006 · 破坏性 DDL 的依赖查询放进驱动层，视图依赖允许「多报不漏报」

- 状态：**已采纳**
- 日期：2026-11-03
- 影响面：`driver.Metadata`（新增方法）、`model.Dependent`、`internal/driver/{sqlite,mysql,pg}`、`service/ddl`、`internal/api`

---

## 背景

`docs/07` §5.1 要求「重命名/删除对象前查询引用关系」，验收项 8 具体到「存在视图依赖时 `DROP TABLE` 被阻断」。

问题是「引用关系」在三个引擎里的可得性完全不同：

| 依赖类型 | SQLite | MySQL 5.7 | PostgreSQL |
|---|---|---|---|
| 入向外键（谁引用我） | `PRAGMA foreign_key_list` 逐表扫 | `information_schema.key_column_usage` 精确 | `information_schema.referential_constraints` + `key_column_usage` 精确 |
| 视图 → 表 | `sqlite_master.sql` 文本匹配 | `view_table_usage` 在 5.7 上不可靠，只能回落到 `views.view_definition` 文本 | `information_schema.view_table_usage` 精确 |

三条路可走：

1. **在 `service` 里按引擎分支**：直接被 ADR-0005 否掉，而且会让「新引擎」这件事重新变成改 `switch`。
2. **一律文本搜索**：三个引擎行为一致、实现最省，但入向外键这种引擎已经给了精确答案的信息被降级成猜。
3. **扩一个元数据方法，每个引擎用自己最好的手段**：驱动各自给出「能精确就精确、不能精确就文本」，并把可信度带出来。

---

## 决策

采用第 3 条：`driver.Metadata` 新增

```go
Dependents(ctx context.Context, ref model.ObjectRef) ([]model.Dependent, error)
```

返回的每一项都带 `Exact`：

```go
type Dependent struct {
    Kind   string // foreign_key | view
    Name   string // 约束名或视图名
    Detail string // 人话，例如 "orders.customer_id → customers.id"
    Exact  bool   // false 表示靠文本匹配得到，可能误报
}
```

### 1. 精确与近似的边界画在这里，不画在调用方

- **入向外键**：三个引擎都能精确回答，`Exact = true`。
- **视图依赖**：MySQL 5.7 / SQLite 只能文本匹配，`Exact = false`；PostgreSQL 走 `view_table_usage`，`Exact = true`。
- **例程依赖**：本次不做。文本搜索例程体误报率太高，`docs/07` §5.1 自己也标注了「不可靠，需标注」；等有确切需求再加，且必须带 `Exact = false`。

### 2. 视图依赖宁可多报，不可漏报

文本匹配用**词边界**（`\b<tbl>\b`，大小写不敏感，表名里的正则元字符先转义）。误报的代价是多一次人工确认；漏报的代价是删掉一张被视图引用的表，`DROP TABLE` 之后视图查询才报错，且**没有回滚**。

因此：

- 视图依赖命中 → **阻断**删表，并把命中的视图名列出来；
- `Exact = false` 的项在提示文案里显式标注「按文本匹配得到，可能误报」。

### 3. 阻断在生成预案时做，不在执行时做

依赖查询是只读的元数据查询，放在 `BuildDDLPlan` 里：错误能在用户点「执行」之前暴露，且预案本身就是一次快照——执行前还会用表定义哈希兜一次「别人中途改了结构」。

### 4. `drop_column` 只按精确信息阻断

删列时，视图是否引用了**那一列**无法从文本可靠判断（视图体里可能只在 `SELECT *` 中隐式包含）。所以：

- 引用该列的**入向外键** → 阻断（这是精确的，`key_column_usage` 有列名）；
- 视图 → 不在删列时阻断，只在预案的 `note` 里提示「该表有 N 个视图依赖，删列前请自查」。

这条是**有意的不对称**，写在文档里而不是藏起来：宁可让删列少挡一次，也不要用一个猜出来的规则把正常操作挡死。

---

## 后果

- 新增一个引擎时，只需实现 `Dependents`；`service` 与 `api` 不知道引擎名，ADR-0005 的分层不被破坏。
- `drivertest` 增加一致性用例：造一个「父表 + 子表 + 引用父表的视图」，三引擎都必须报出这两条依赖，且外键那条 `Exact = true`。
- 删表从「只能写原始 SQL」变成设计器里的一个动作，但它**必然不可逆**：`ROLLBACK` 只有列结构，数据回不来。预案里标 `Irreversible`，且回滚语句只是补偿（重建空表）。
- 已知限制：例程依赖未覆盖；MySQL 5.7 的视图依赖可能漏报（`view_table_usage` 为空时回落文本匹配，若视图用了 `db.schema` 前缀等写法仍可能漏）。这一条写进 `docs/07` §8.2。
