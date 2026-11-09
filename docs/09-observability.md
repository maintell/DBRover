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

> **v1 实现口径**：巡检报告由 `service.assess` 产出，实际只评估「连接占用（>80% warn / >90% crit）、锁等待（按等待**条数**而非链长，>3 crit）、缓存命中率（<90% warn）、慢查询（有窗口增量即 warn）、表体量（单表 >1GB）」。上面「最长事务 > 5 分钟」与「慢查询占比」两条**未实现**：前者要引擎侧事务起始时间，后者要语句总耗时，分界见 §9.2。另外此处是**面板巡检结论**的阈值（30%），§7 是**本地告警**的阈值（50%），两套独立规则、各自尚未实现的那一条也在 §9.2 分别登记，不要混成同一个数字。

### 3.2 慢查询

> **实现口径**：面板只给「总耗时 / 次数 / 平均 / 行数」四列；下面表格里的 P95、最大单次、首次/末次出现**尚未实现**（见 §9.2）。

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

> **v1 现状**：上面这张表是目标形态（11 个检查项、阻断/警告/提示三级、`checks.yaml` 自定义项）。
> 实际实现是 **四路采集**：连接与缓存 / 锁等待 / 慢查询 / 表体量（`service.Executor.HealthCheck` → 纯函数 `assess`）。
> 报告里**固定 4 项**：每项按「拿不拿得到数据」落到 `ok` / `warn` / `crit` / `unknown`，
> 整体取最严重的一项；**`unknown` 绝不算 ok**（否则「没看到问题」会被冒充成「没有问题」）。
> 「缓存命中率」不是独立一项——它是嵌在「连接与缓存」分支里的，引擎没有性能指标时
> 整段只会产出 1 条 `unknown`。验收：`scripts/accept/monitor-degrade.ps1` 在 SQLite 上断言
> 四项全为 `unknown`、整体不是 `ok`，且三个不支持的端点返回 400 + 点名引擎。

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

---

## 9. 实现状态（滚动更新）

与 `docs/06` / `docs/07` 同一套规则：上面写的是**目标契约**，这里写「已经做到哪一步」。两者不一致即缺陷——要么补代码，要么把契约降级并说明理由。

### 9.1 已实现

| 能力 | 契约位置 | 实现 | 验证 |
|---|---|---|---|
| 连接与负载 / 缓存命中 | §3.1 的两张卡 | `driver.MetricsProvider`（可选接口，判据是 `Capabilities().Metrics`）+ `service.Executor.Metrics`。PG 走 `pg_stat_database`（**按 `current_database()` 收窄**——它是集群级的，把所有库加起来会得到一个看不出是哪个库在打磁盘的混合命中率）+ `pg_settings.max_connections`；MySQL 走一次 `SHOW GLOBAL STATUS` 挑三项 + `@@max_connections`。**结论由规则产出**：连接数过八成上限、缓存命中率低于 90% 各触发一条警告 | `TestSummarizeCountersRules`（四种构造数据 + 命中率算法 + 「两边都是 0 时不给命中率」）、`drivertest` 的 `MetricsReported`（真机：上限为正、连接数含自己这条、**缓存计数不倒退**）、`TestUIMetricsPanel` |
| 采集超时降级 | §2.1 / 验收 8 | 采集带 3 秒超时（`metricsCollectionTimeout`）。超时**不报错**：返回 200 + `degraded` 说明，界面照常画面板并标注。理由：监控拿不到数字不该看起来像库挂了 | `TestCollectMetricsDegradesOnTimeout`（假 provider 阻塞 → 断言不返回错误、标注降级、不编造读数）、`TestCollectMetricsPassesThrough` |
| 面板首屏的取数方式 | §3.1 / 验收 1 | 六块数据（指标 / 慢查询 / 表体量 / 锁 / 趋势 / 告警）**并发发**：串行等于把「最慢的那个 × 6」当成首屏时间，而验收要求 2 秒内出结论——远端库上每个端点都可能要 1 秒。`Promise.allSettled` + 逐块降级，只有指标本身是必须的 | `TestUIMetricsPanel`（钉住 `Promise.allSettled` 与逐块取值）；真机 MySQL：六个端点串行合计 37–50ms（本机），改造前后六块都完整渲染 |
| 巡检报告导出 | §5 / 验收 12 | `?format=markdown` 返回 `text/markdown` 的表格：级别写中文（贴进工单能直接读），结论里的竖线转义、换行替换成空格——一条带 `|` 的语句足以把整张表拆坏。界面「导出 Markdown」按钮在新窗口打开它 | `TestHealthMarkdownEscapesAndLabels`（竖线、换行、中文级别）；真机 PG：`Content-Type: text/markdown; charset=utf-8` 且表格五行齐全 |
| 空闲事务高亮 | §3.3 / 验收 5 | 会话列表里 `idle in transaction` 且已持续 >60 秒的行标红，并挂一条 title 说明「它什么都不干却抱着锁」。**只有 PG 会给这个状态**（MySQL 的 processlist 只有一个 `Sleep`，看不出来），所以有数据就标、没有就不标，不做假 | `TestUISessionsPanel`（条件与文案）；真机 PG 17：`BEGIN; SELECT 1;` 后挂着不动 → 1m33s 时该行确实标红（截图确认） |
| 本地告警（两条规则） | §7 / FR-MON-08 | `alertState` 记住「连续几次」与「上次触发时间」，规则 1 连接占用**连续 3 次** >80%（抑制 30 分钟）、规则 2 **阻塞链深度 >3**（抑制 5 分钟，深度由边算最长路，环路用访问集合挡住）。状态**只在内存**、只在评估时前进：关掉应用就不再盯着，响应里带 `note` 讲明这点 | `TestAlertRules`（第 3 次才触发、抑制窗口内不重复、过了窗口再响、中间掉下来会清零）、`TestChainDepth`（单边 / 两级链 / 一对多 / 纯环不死循环）；**真机 MySQL 5.7**：把测试容器的 `max_connections` 压到 1 让占用率变 100% → 第一次 `/monitor/alerts` 返回 `连接占用 / 连接占用连续 3 次采样超过 80%（本次 100%）`，**随后两次都返回 0 条**（30 分钟抑制生效）；内置浏览器两种状态都看过——无告警时显示「告警（0）本次没有触发告警。告警只在应用运行时生效…」 |
| 巡检（一键健康检查） | §5 / FR-MON-07（部分） | `service.Executor.HealthCheck` 汇总已有四路采集（指标 / 锁 / 慢查询 / 体量），`assess` 是纯函数、便于构造数据断言规则。分级：`crit` = 现在就有事、`warn` = 再走一步就出事、`unknown` = 这项拿不到数据——**unknown 绝不算 ok**，否则等于把「没看见问题」冒充成「没问题」。单项失败不影响整份报告（巡检最怕「一项挂了整页白」，那正是在最需要它的时候罢工） | `TestAssessHealthRules`（全绿 / 有锁等待升 warn / 采集失败落 unknown 且带原因 / 首次采样的慢查询给 unknown）、`TestUIMetricsPanel`（按钮与级别文案）；真机见下 |
| 本地指标留存（7 天） | §4 / 验收 11（部分） | 单文件 SQLite（`modernc.org/sqlite`，纯 Go），表结构照 §4 的 `metric_sample`；**按分钟归并**（面板每 5 秒轮询，不归并一小时就是 720 行）、**只在面板读的时候写**、`INSERT OR REPLACE`、**每天最多清理一次**（7 天前）。`serve -metrics <文件>` 开启，不指定就不留存，历史端点返回 `available=false` 而不是假装有数据。ADR-0016 | `TestMetricStoreRecordAndSeries`（同一分钟只留最后一条、按连接隔离、8 天前的点被清掉）；真机：面板开两次 → `/monitor/history` 有点 |
| 表体量 Top N + 增长 | §3.5 / FR-MON-06 | `driver.CapacityProvider`（判据 `Capabilities().Capacity`）。PG 用 `pg_table_size` / `pg_indexes_size` / `pg_total_relation_size`——都是 **O(1) 的目录计算，不扫表**；MySQL 用 `information_schema.TABLES` 并**排除系统库**（它们总是排在前几名，会把业务表挤掉）。数据与索引分开给：只报总量的话，「为什么这张表这么大」还得再点进去看一次。**增长未做**：需要本地时序留存（FR-MON-05） | `TestCapacityCapabilityIsHonest`（SQLite false / 两引擎 true）、`TestUIMetricsPanel`（界面接线）；真机 MySQL 见上 |
| QPS | §3.1 / FR-MON-01 | `driver.Counters.Statements`（累计语句数，-1 表示这个实例没给）+ 服务层第二份采样状态，两次相减除以时间跨度。MySQL 取 `Questions`；PG 取 `SUM(calls)`（**要有 pg_stat_statements**，没装时留 -1，界面不显示这一行而不是显示 0）。首次采样不给数字、计数器重置时跳过——与慢查询同一套规矩 | `TestApplyQPSSkipsFirstSample`（首次不给、2 秒 200 条 = 100 QPS、清零后跳过）；真机 MySQL：第二次采样出现 QPS |
| 终止会话的确认与审计 | §3.3 / 验收 6 | 确认框带**该会话当时在跑的语句**（超 400 字符截断，并**先转义再拼 HTML**——语句来自数据库，不能当可信文本拼）；终止**记审计**：`action=monitor.kill_session`、`risk=R4`、`friction=F2`、`sql` 是被终止会话当时的语句（已脱敏），失败时 `outcome=error` 且带原因。终止**不走执行门**（它不是 SQL，走的是驱动的专门方法），审计在 API 层自己写，id 复用同一个生成器以免审计里出现两种 id 形态 | `TestUISessionsPanel`（确认框含语句、含转义函数）；真机 MySQL：后台跑 `SELECT SLEEP(90)` → 终止 → `/audit` 出现 `monitor.kill_session` 且 `sql=SELECT SLEEP(90)`；内置浏览器确认框显示同一句 SQL |
| 面板刷新与停止 | §2.1 / 验收 10 | 打开面板拉一次快照，之后每 5 秒一次；停止由 `MutationObserver` 盯「`#metrics-panel` 从 DOM 消失」立即触发 `stopMetricsRefresh()`，**不再只靠定时器下一次 tick 自检**（那会切走后多打两批请求，真机实测抓到并修掉）。定时器的自检保留为第二道 | `TestUIMetricsPanel`（断言 observer 接线 + `clearInterval`，含变异验证）；真机 MySQL + 内置浏览器：切走后 13 秒零新 `monitor/metrics` 请求 |

### 9.2 尚未实现

| 缺口 | 契约位置 | 影响 | 处置 |
|---|---|---|---|
| 阻塞链的树形视图与 metadata lock | §3.4 / FR-MON-03 | 只有边、界面上只有平表；**metadata lock 那一层没有** | 树可以由边算出来（按 `blocking_id` 串），留到界面重做时一起；MDL 要 MySQL 8 的 `performance_schema.metadata_locks`，与 `docs/07` 验收 11 是同一个卡点。**锁等待边本身在 MySQL 8 曾整块坏掉**：驱动只查 `information_schema.innodb_lock_waits`（8.0 已移除），已改为按 `SELECT VERSION()` 主版本分派到 `performance_schema.data_lock_waits`（真机 8.0.39 实测，见 docs/04 §锁等待行）。MDL 层仍未做 |
| 容量增长的可视化 | §3.5 / FR-MON-06 | 增长的**数值**已给出（每次读体量时顺手存进留存，按最早点算差值与跨度）；**没有曲线图**，也没有「按天聚合」 | 曲线要前端画图，等正式前端；聚合要等比降采样，等有「看一周」的真实需求 |
| 巡检的自定义检查项与 Runbook 编排 | §5 / 验收 12 | 四路采集（连接与缓存 / 锁等待 / 慢查询 / 表体量）与报告导出（`?format=markdown`）都已实现；**没有「自定义检查项」与「按 Runbook 编排」** | 自定义项要先把检查项做成数据（而不是代码分支）；编排要先把 Runbook 定义落成文件 |
| 告警的另两条规则与外部推送 | §7 / FR-MON-08 | 已做「连接占用连续 3 次 >80%」与「阻塞链深度 >3」；**>5 分钟长事务**与**慢查询总耗时占比 >50%** 未做，也没有外部推送（v1 明确不做） | 长事务：`Metadata.Blockers`（DDL 预检链路）已在 MySQL 侧查 `innodb_trx.trx_started` 并给出持续秒数，但**监控告警 `Alerts` / 巡检 `assess` 没有接这条链路**（它们是另一套采集，不调 `Blockers`），所以要接的是「把已有会话观察接进告警评估」，不是「驱动拿不到数据」；占比要先有引擎侧的语句总耗时 |
| 慢查询的 P95 / 最大单次 / 首末次 | §3.2 的列表头 | §3.2 的表格承诺了「P95、最大单次耗时、首次/末次出现」，实现只给了总耗时 / 次数 / 平均 / 行数——**那三列没有**。另外「按窗口内总耗时重排」只在驱动按**累计**取回的 TOP-N 子集内成立：新近爆发但历史排名靠后的语句根本不在结果里 | 补字段要驱动侧多给（PG 的 `stddev_exec_time` / `max_exec_time`；MySQL 的 `SUM_*` 需要更多列）；「新近爆发」需要按窗口取数而不是按累计取数，属于采样策略变更 |
| 慢查询的小时聚合 | §4 的 `slow_query_hourly` | 表已设计但没写：面板给的是**窗口增量**，没有按小时落盘 | 与本片的 `metric_sample` 同源，加一张表 + 一个聚合任务即可，等有「看一周慢查询」的真实需求再做 |

**验收现状（2026-11-05 真机补验后）**：12 项里 2、3、4、5、6、8、11、12 已由真实用例或真机验证覆盖；

- **第 1（面板 2s 内出首屏结论）**：真机 MySQL 5.7（debug-ubuntu 一次性容器）+ 内置浏览器实测，
  一次点开「指标」即同时给出六块真实数据（连接与负载/缓存命中/QPS 规则结论、慢查询 Top 8 含
  「首次采样，暂无增量」标注、表体量、锁等待、趋势、告警），**结论由规则产出而不是固定文案**。
  数据齐备由「六端点 `Promise.allSettled` 并发取」保证（本机六端点合计 37–50ms）；
  仍缺一条纯 `click→render` 的自动化毫秒断言（浏览器工具的往返计入侵扰），如实标注为部分。
- **第 10（关闭面板后停止采集）**：**已由真机网络证据验证**。此前只靠 `setInterval` 回调里的自检，
  真机实测抓到缺陷：切走视图后 `#metrics-panel` 立即不在 DOM，但仍多发了两整批 6 端点请求
  （序列 62/68，约 10 秒）。根因是自检要等下一次 5 秒 tick，而在飞请求与下一个 tick 会继续打库。
  修法：加 `MutationObserver` 盯「面板从 DOM 消失」这件事本身，一消失立即 `stopMetricsRefresh()`，
  不再依赖 20 处 `results.replaceChildren` 散点各自记得清理。修后同一操作复验：切走后 13 秒内
  `monitor/metrics` **零新请求**（序列停在 118）。门禁 `TestUIMetricsPanel` 已升级断言 observer 接线
  并做变异验证（去掉 `watchMetricsPanel()` 调用即红）。

第 7 只有等待关系与链深（没有树形与 MDL 层）；第 9 卡在 Oracle 实例。
