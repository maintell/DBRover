# 09 · 性能监控与运维

> 需求编号：`FR-MON-01..08`。核心命题：**用户要的不是图表，是「问题在哪」。**

---

## 1. 原则

1. **先回答「哪里有问题」，再给数据。** 面板首屏是结论（Top 慢 SQL、阻塞链、异常会话），不是一堆折线图。
2. **不侵入生产。** 监控查询必须轻量、可限流、可关闭；绝不做全表扫描式采集。
3. **不假设权限。** 所有监控视图都可能无权访问（`pg_stat_statements` 未装、AWR 需 License）。缺失时明确降级，不报错。
4. **不假装精确。** 估算值标注「估算」，采样值标注「采样 N%」。

---

## 2. 数据来源与成本

| 引擎 | 指标源 | 慢查询源 | 成本 | 备注 |
|---|---|---|---|---|
| PG | `pg_stat_database`、`pg_stat_activity` | `pg_stat_statements`（需扩展） | 低 | 未装扩展时提示安装语句 |
| MySQL | `SHOW GLOBAL STATUS`、`performance_schema` | `events_statements_summary_by_digest` | 中 | 大实例 `information_schema` 慎用 |
| MSSQL | `sys.dm_os_performance_counters`、`dm_exec_requests` | Query Store（需启用） | 中 | 未启用时退化为 `dm_exec_query_stats` |
| Oracle | `v$sysstat`、`v$session` | `v$sqlstats` / `v$sql`（AWR 需付费） | 中 | **不得默认查 AWR** |
| SQLite | 无 | 无 | — | 隐藏相关面板 |
| Redis | `INFO` | `SLOWLOG GET` | 低 | 注意 `SLOWLOG LEN` 可能很大 |
| Mongo | `serverStatus`、`$currentOp` | `system.profile`（需开 profiling） | 中 | profiling 有性能开销，需用户显式开启 |
| ES | `_cluster/health`、`_nodes/stats`、`_cat/indices` | 慢日志配置（需索引级开启） | 低 | `_cat` 用 `?format=json` |

### 2.1 采集策略（防止「监控把库压垮」）

| 场景 | 策略 |
|---|---|
| 面板打开时 | 拉一次全量快照 |
| 面板保持打开 | 每 5 秒拉增量（仅轻量指标） |
| 面板关闭 | 停止采集（不做后台轮询） |
| 慢查询聚合 | 每 30 秒一次，且结果做缓存 |
| 大表体积统计 | 手动触发（一次可能扫几千行系统表） |
| `performance_schema` 查询 | 加超时 3s，超时即放弃并降级 |

**统一约束**：所有采集查询带 `timeout`（默认 3s）与 `LIMIT`。超时不算错误，记一次「降级」并在 UI 标注。

---

## 3. 面板设计

### 3.1 首屏：健康摘要

四张卡片，每张给出「状态 + 一句结论 + 可点进详情」：

```
┌─ 连接与负载 ────────┐ ┌─ 慢查询 ──────────────┐
│ 活跃 12 / 上限 200  │ │ 近 15 分钟 3 条指纹   │
│ ● 正常              │ │ ● 1 条总耗时 > 10s    │
└─────────────────────┘ └───────────────────────┘
┌─ 锁与阻塞 ──────────┐ ┌─ 容量 ────────────────┐
│ 2 个会话在等待      │ │ 最大表 4.2GB（order） │
│ ● 有阻塞（4.1s）    │ │ ● 磁盘余量未知        │
└─────────────────────┘ └───────────────────────┘
```

「结论」必须是**规则产出**而非固定文案：

- 活跃连接 / `max_connections` > 80% → 警告
- 存在阻塞链长度 ≥ 2 → 警告
- 最长事务 > 5 分钟 → 警告
- 慢查询总耗时 / 总耗时 > 30% → 警告
- 缓存命中率 < 90%（MySQL InnoDB buffer pool、PG `blks_hit/(blks_hit+blks_read)`）→ 警告

### 3.2 慢查询

**指纹聚合**（`sql_hash`）后展示：

| 列 | 说明 |
|---|---|
| 指纹摘要 | 归一化 SQL 前 80 字符 |
| 调用次数 | — |
| 总耗时 / 平均 / P95 | 按总耗时降序是默认排序（**不是**按平均） |
| 最大单次耗时 | 找尖刺 |
| 行数（平均 / 最大） | 判断是否返回过多 |
| 首次/末次出现 | 判断是新问题还是老问题 |
| 操作 | 展开样本、看计划、加收藏、复制到编辑器 |

**默认排序按总耗时**：一条 20ms 但每秒跑 1000 次的查询，比一条 5s 跑 1 次的查询危害大得多。按平均耗时排序是最常见的误用。

**详情抽屉**：

- 该指纹的调用趋势（按分钟/小时聚合）
- 3 条样本（带实际参数，参数值脱敏可配）
- 关联的执行计划（若能取到）
- 「对比历史」：与 7 天前同时段比较

### 3.3 会话

| 列 | 来源 |
|---|---|
| 会话 ID | 引擎标识（PG `pid`、MySQL `thread_id`、Oracle `sid,serial#`、MSSQL `session_id`） |
| 用户 / 应用名 | 连接属性（MySQL `USER`、PG `application_name`、MSSQL `program_name`） |
| 来源 IP | 客户端地址 |
| 状态 | `active` / `idle` / `idle in transaction` / `waiting` |
| 已运行时长 | 当前语句耗时（**不是**连接时长） |
| 当前 SQL | 截断 200 字符，可展开 |
| 等待事件 | PG `wait_event_type/wait_event`、MySQL `STATE`、MSSQL `wait_type` |
| 阻塞者 | 谁在阻塞我（若可得） |

**必须高亮**：

- `idle in transaction` 超过 60 秒 → 红色（这是最容易被忽视的阻塞源）
- `active` 超过 30 秒 → 黄色
- 正在等待锁 → 橙色，并显示阻塞者

**Kill 操作**：

- 需确认，展示将被终止的 SQL 摘要。
- `R2` 风险（会中断业务），记审计。
- 记录发起人、时间、目标会话、结果。
- 提供「先取消当前语句」与「终止连接」两种（PG: `pg_cancel_backend` vs `pg_terminate_backend`），默认前者（影响更小）。

### 3.4 锁与阻塞链

以**树**展示 `谁 → 等 → 谁`：

```
会话 412 (UPDATE order, 12.4s)  ← 持锁
   └─ 会话 587 (UPDATE order, 4.1s)  等待 row lock
   └─ 会话 622 (ALTER TABLE order, 0.8s)  等待 metadata lock
```

- 根节点（持锁者）高亮。
- 点击任一节点可看其 SQL 与会话详情。
- 提供「终止根节点」快捷动作（需确认）。
- 无法构建时（引擎不支持）显示「该引擎不支持阻塞链解析」，并降级为「等待事件列表」。

### 3.5 容量

| 视图 | 内容 |
|---|---|
| 库/表体积 | 按大小降序，Top 50 + 「其余合计」 |
| 增长趋势 | 需本地留存历史（见 §4） |
| 索引体积占比 | 找出「索引比数据还大」的表 |
| 空闲空间 | 仅 PG（`pg_stat_user_tables.n_dead_tup`）与 MySQL（`data_free`） |

---

## 4. 本地时序留存（`FR-MON-05`）

不做 TSDB，用 SQLite 两张窄表：

```sql
CREATE TABLE metric_sample (
  connection_id TEXT NOT NULL,
  ts            INTEGER NOT NULL,   -- unix 秒，按分钟对齐
  metric        TEXT NOT NULL,      -- 'qps','conn_active','conn_idle','cache_hit','tps'
  value         REAL NOT NULL,
  PRIMARY KEY (connection_id, metric, ts)
) WITHOUT ROWID;

CREATE TABLE slow_query_hourly (
  connection_id TEXT NOT NULL,
  hour_ts       INTEGER NOT NULL,
  sql_hash      TEXT NOT NULL,
  calls         INTEGER NOT NULL,
  total_ms      INTEGER NOT NULL,
  max_ms        INTEGER NOT NULL,
  PRIMARY KEY (connection_id, sql_hash, hour_ts)
) WITHOUT ROWID;
```

- 只在面板打开时采样（每分钟一条），所以数据是「用户关心的时候才有」，这不完美但对单机工具是合理取舍。
- 保留 7 天，后台清理（每日一次）。
- 写入用批量提交，避免频繁 fsync。

**为什么不做常驻采集**：常驻会持续对生产库发查询，且用户在没打开面板时并不需要趋势。要真正常驻，那是监控平台的事，不是工具的事。

---

## 5. 巡检 Runbook（`FR-MON-07`）

一组预置检查，一键跑完出报告：

| 检查项 | 判定 | 适用引擎 |
|---|---|---|
| 无主键表 | 存在即警告 | 全部关系库 |
| 缺索引的外键 | 存在即警告 | PG/MySQL/MSSQL |
| 重复索引（前缀重叠） | 存在即提示 | 全部 |
| 未使用的索引 | `idx_scan = 0` 且存在 > 7 天 | PG（MySQL 需 `sys.schema_unused_indexes`） |
| 表膨胀 / 死元组 | 死元组比 > 20% | PG |
| 长事务 | 存在 > 5 分钟 | 全部 |
| 连接数接近上限 | > 80% | 全部 |
| 复制延迟 | > 10s | PG/MySQL |
| 备份是否存在且新鲜 | 本地记录 > 7 天未备份 | 全部 |
| 字符集混用 | 同库表排序规则不一致 | MySQL |
| 大表无分区 | 单表 > 100GB | 全部 |

**报告形态**：分「阻断 / 警告 / 提示」三级，每条含「发现 → 影响 → 建议动作」，可导出 Markdown。

**可扩展**：巡检项以配置声明（`checks.yaml`），允许用户自定义 SQL 检查（只读，带超时）。

---

## 6. 采集实现细节

### 6.1 SQL 指纹归一化

复用 `dialect.Normalize`（见 04 文档 §4.3）。但监控场景要用**引擎自己的指纹**（`pg_stat_statements.queryid`、MySQL `DIGEST`、MSSQL `query_hash`）作为主键，`Normalize` 只用于「本工具内部的历史聚合」。

两者不通用：引擎指纹包含更多信息（如常量类型），本工具切片可能更粗。展示时以引擎指纹为准，跨引擎比对用本工具指纹。

### 6.2 时间窗口

- 慢查询默认「近 15 分钟」，可选 1h / 24h / 7d。
- `pg_stat_statements` 是**累计值**，不是窗口值 → 必须**差分**计算。第一次拉取只能给累计值，第二次起给增量。UI 必须标注「首次采样，暂无增量」。
- MySQL `events_statements_summary_by_digest` 同理。
- Query Store 有时间维度，可直接按窗口查（优势明显）。

**这是最容易被做错的地方**：把累计值当窗口值展示，会让用户看到「这条 SQL 跑了 120 万次」而实际是自实例启动以来。

### 6.3 脱敏

- 慢查询样本可能含手机号/身份证。默认把样本参数替换为 `?`，提供「显示参数」开关（按连接配置）。
- 会话 SQL 同理。
- 审计中不记录参数值。

---

## 7. 告警（`FR-MON-08`，P2）

仅本地通知，不做外部推送（v1）。

| 规则 | 触发 | 抑制 |
|---|---|---|
| 连接数 > 80% | 连续 3 次采样 | 30 分钟内不重复 |
| 存在 > 5 分钟长事务 | 检测到即触发 | 同一会话只报一次 |
| 阻塞链 > 3 | 检测到即触发 | 5 分钟 |
| 慢查询总耗时占比 > 50% | 连续 2 次 | 1 小时 |

**只在应用运行时生效**，UI 需说明。

---

## 8. 验收清单

| # | 项 | 判定 |
|---|---|---|
| 1 | 面板打开后 2s 内出首屏结论 | 手动 |
| 2 | 首屏结论由规则产出，非固定文案 | 单测（构造数据断言规则） |
| 3 | `pg_stat_statements` 累计值做差分，标注首次采样无增量 | 集成 |
| 4 | 慢查询默认按总耗时排序 | 单测 |
| 5 | `idle in transaction` > 60s 红色高亮 | 单测 |
| 6 | Kill 会话前的确认含 SQL 摘要，且记审计 | 集成 |
| 7 | 阻塞链树正确（三分层：根、等待者、metadata lock） | 集成（构造锁） |
| 8 | 采集查询全部带 3s 超时，超时降级不报错 | 单测（mock 慢查询） |
| 9 | AWR 不可用时静默降级且注明 | 集成（Oracle） |
| 10 | 关闭面板后停止采集 | 集成（断言无新查询） |
| 11 | 指标留存 7 天后自动清理 | 单测 |
| 12 | 巡检报告可导出且分级正确 | 手动 |
