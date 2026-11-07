# 13 · 状态机（UI 可视状态与合法转换）

> 状态集合、合法转换、触发事件、执行角色、前后置条件、守卫。每个关键状态都有合法进入与退出路径，无孤岛/死端。业务状态以 `docs/03`、`docs/05`、`docs/06` 为准。

## 1. 连接状态机

| 状态 | 合法进入 | 合法退出 | 触发 | 守卫 |
|---|---|---|---|---|
| 未连接 | 启动/退出后 | selectConnection | 选/新建连接 | 令牌就绪 |
| 令牌待验 | serve 模式打开 | 进入令牌门 `#gate` → `btn-token` | URL `#t=` 或注入量 `window.DBRoverBootstrapToken` | `bootstrap.js` |
| 已连接 | selectConnection | 切换连接/删除连接/退出 | `state.current` | 能力位 `applyEngineLayout` |
| 连接不可用 | testConn 失败 | 测试/删除/重连 | `conn-test.err` 反馈 | — |

## 2. 编辑器 / 执行状态机

| 状态 | 进入 | 退出 | 守卫/证据 |
|---|---|---|---|
| 编辑器就绪 | 选标签/输入 SQL | 执行/计划/取消 | `#sql`、`ui2.tabs` |
| 执行中 | `runSQL(false` or `false,true`)` | 成功/失败/取消 | `startQueryTimer`、`SLOW_QUERY_SECONDS=3`、`PROMPT_SECONDS=30`（`slowquery_test.go`） |
| 执行中·待决定是否继续 | 运行 30 秒 | 继续等 / 取消 | `PROMPT_TITLE` 弹窗、`if (!keep && stillRunning) await cancelRunning();` |
| 有结果 | 成功 | 新执行/切标签/清空/改数据 | `renderExec` 每条 `result-block`（`exec_blocks_ui_test.go`） |
| 失败块 | 任一句失败 | 改语句重试/跳回对应语句 | `d.textContent = '失败：' + r.err` + `jumpToStatement` |
| 已取消 | `cancelRunning` | 回到编辑器就绪 | 引擎侧语句终止（`docs/05` 验收 9） |
| 待提交变更集 | attachEdit/insert/delete/copy | 提交/丢弃 | `#pending-bar`、`td.dirty`（`style.css`） |
| 预案预览 | submitChanges | 批准 apply / 拒绝 / 冲突 | 前后对比 + 回滚 SQL；R3/R4 二次确认（`FR-SEC-04`） |
| 已提交 | apply 成功 | 一键撤销 / 新变更 | 结果被 service 持久化 + 审计 |

`finally` 顺序守卫：`} finally {\n    stillRunning = false;\n    stopTimer();`（`slowquery_test.go` 钉死，异常路径也必须停表）——重设计不得改动。

## 3. 功能域导航状态机（重设计新增，纯 UI）

| 状态 | 进入 | 退出 | 守卫 |
|---|---|---|---|
| `domain=data`（默认） | 应用启动/点「数据」 | 点其他域 | `state.current` 保留 |
| `domain=structure` | 点「结构」 | 点其他域/返回数据 | 能力位决定子项显隐 |
| `domain=monitor` | 点「监控」 | 点其他域/关闭面板 | `caps.metrics` 等；离开即停采集 |
| `domain=agent` | 点「AI」 | 点其他域 | 会话按连接隔离 |
| `domain=ops`（运维任务） | 点「运维任务」 | 点其他域 | **界面未接入**：显示「CLI/API 可用、界面未接入」空状态 + 去文档/CLI 指引（D6） |

守卫：所有域切换 `switchDomain(target)` 必须 (a) 保留 `state.current`，(b) 不重建 `#sql`/`#results` 节点身份（否则 `ui2.js` 的活 Dom 引用与 `renderExec` 包装失效），(c) 不改变任何 `caps` 判断。

## 4. 数据状态流转六类动词（创建→修改→审核→支付/履约→完成→取消）

映射（数据库工作台语境）：
- 创建 = 新建连接 / 新建 SQL 标签 / 新建表 / 插入行。
- 修改 = 编辑 SQL / 改单元格 / 结构设计填表。
- 审核 = 变更预案 `plan` + 审批门（R2+ Agent 审批卡片、R3/R4 二次确认）。
- 支付/履约 = `apply`（执行门落库、写审计）。
- 完成 = 结果网格可见 + 明确成功反馈 + 下游可读（对象树/结构刷新）。
- 取消 = `cancelRunning` / 丢弃 `pending` / `rollback`（一键撤销）/ 关闭标签。

不可逆迁移标注：`apply` 落库、`killSession`、`drop_table`、`planDropColumn` 不可逆 → 必须 `danger` 红提示 + 确认 + 回滚语句；`switchDomain`、切标签、丢弃 `pending` 可逆。补偿事务：一键撤销（`docs/06` FR-DATA-07）与回滚 SQL（`rollbackNote`）。
