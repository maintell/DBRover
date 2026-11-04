# ADR-0014 · 迁移注释：PG 表与列都搬、MySQL 只搬表、失败不拦数据

- 状态：**已采纳（已真机验证）**
- 日期：2026-11-05
- 影响面：`internal/service/migrate.go`（`BuildMigrationCommentDDL` / `quoteLiteral`）、`cmd/dbrover`（`migrate run` 收尾）、`docs/08 §7`
- 关系：**替代** [ADR-0012](0012-migrate-constraints-and-defaults.md) §3 的「注释不搬」

---

## 背景

ADR-0012 §3 决定注释不搬，只在 `BuildMigrationNotes` 里说一声。真机走一遍之后这条不成立：

- 注释是「这张表是什么」的唯一载体。迁移过去以后数据对、表能查，但接手的人面对一堆
  没有说明的表——跨引擎迁移往往正是交接场景，最需要说明的时候恰好丢了说明。
- 「表/列注释未迁移」这句提示把活推回给用户：几百列注释手工抄不现实，等于事实上放弃。
- 三个引擎写表注释都只要一条语句，成本比当初估计的低得多。

## 决策

### 1. PostgreSQL：表注释与列注释都搬

```sql
COMMENT ON TABLE "t" IS '表的说明';
COMMENT ON COLUMN "t"."a" IS '列的说明';
```

`obj_description` / `col_description` 读得到的，`COMMENT ON` 就写得回去，一一对应，
不存在语义漂移。

### 2. MySQL / MariaDB：只搬**表**注释

```sql
ALTER TABLE `t` COMMENT = '表的说明';
```

列注释没有独立语句：MySQL 要求 `MODIFY COLUMN <完整列定义> COMMENT '…'`，也就是必须
重算类型、可空、默认值、字符集、生成列表达式……**其中任何一处重算错，改的就不是注释
而是列本身**。为一句说明去冒改表结构的风险不划算，v1 跳过并说明。

真机核对：MySQL 5.7 没有 `ALTER TABLE … ALTER COLUMN … COMMENT` 这种写法。

### 3. SQLite：没有注释语法，跳过并说明

### 4. 顺序：数据搬完之后执行，失败只警告

注释排在**数据搬运之后**，与外键同属收尾段（外键之后）。理由与外键不同：

- 外键必须在后，是因为先建约束会挡住父子插入顺序；
- 注释必须在后，是因为**它不该挡住数据**——一条说明写不进去，不该让一次已经搬完并
  校验过的迁移看起来「失败」。

同理，CLI 里单条注释语句失败只打印「注意：注释未迁移（语句）: …」并继续，不 return
error。用户需要知道的结论是「注释没过去」，不是「迁移失败」。

### 5. 字面量按引擎转义，并且有长度上限

单引号双写（`''`）只是半个转义：

- **MySQL** 默认 `sql_mode` 不含 `NO_BACKSLASH_ESCAPES`，反斜杠在字符串里是转义符。
  注释里写 `C:\temp`，存进去的是 `C:` 加一个制表符——**语句不报错，内容已经变了**。
- **PostgreSQL** 从 9.1 起 `standard_conforming_strings=on`，反斜杠就是普通字符，
  多双写一个，存进去就多一个。

所以 `quoteLiteral(engine, s)` 按引擎决定要不要双写反斜杠，而不是「统一双写」图省事。

长度：MySQL 表注释上限 2048 **字符**，超了引擎报 1629 而不是截断。生成语句前先按
rune 计数，超了就跳过并说清超了多少——不发一条注定失败的语句。

## 后果

**正面**

- 迁移之后目标表与源表「看起来一样」，不只是数据一样。
- 注释失败不再把整次迁移判死。
- 反斜杠与单引号不会再被静默改写。

**负面 / 必须说清**

- MySQL 目标端的列注释仍然丢，`migrate run` 会明说「列注释未迁移：MySQL 需要带完整列定义」。
- 注释逐条执行：PG 是 1 条表注释 + 每列 1 条。几百列的大表会多几十条语句，可接受。
- 注释权限不足（PG 要求表 owner 或超级用户）时这些语句会失败。按第 4 条只警告不中断，
  但用户会看到一串「注释未迁移」——这是如实报告，不是噪音。

## 验证计划

1. PG 真机：表与列注释写进真库后用 `obj_description` / `col_description` 读回，中文与
   单引号原样（`TestMigrateCommentsOnRealPG`）。
2. MySQL 真机：表注释写进真库后用 `information_schema.TABLES.TABLE_COMMENT` 读回，
   反斜杠与单引号原样（`TestMigrateCommentsOnRealMySQL`）。
3. 引擎差异：PG 两条语句；MySQL 一条 + 「列注释未迁移」；SQLite 零条 + 「没有注释语法」
   （`TestBuildMigrationCommentDDL`）。
4. 转义与长度：反斜杠在 MySQL 双写、在 PG 不动（`TestQuoteLiteralByEngine`）；超长注释
   跳过且边界值（正好 2048）保留（`TestBuildMigrationCommentDDLTooLong`）。
5. 门禁会真红：把 `quoteLiteral` 的反斜杠分支关掉，第 1、2 条对应的期望值立刻变红
   （实测：MySQL 读回的注释变成 `C:` 加制表符）。
