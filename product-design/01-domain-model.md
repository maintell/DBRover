# 01 · 领域模型（UI 重设计视角）

> 本文件列「会变状态的名词」及其归属模块、唯一标识、生命周期所有者。业务规则以 `docs/03-domain-model.md` 为准，本文件只补 UI 视角的可视对象与归属，不改变业务定义。

## 1. 核心实体

| 实体 | 关键属性 | 唯一标识 | 归属模块（UI 功能域） | 生命周期所有者 | 状态载体 |
|---|---|---|---|---|---|
| Connection 连接 | `id`、`name`、`engine`、`environment`、`access_mode`、`identity`（服务器身份指纹） | `id` | 全局上下文 / 数据（连接抽屉） | service + 连接管理 | `state.current`（`app.js`） |
| Capability 能力位 | `user_types`、`sequences`、`add_routine`、`session_mgmt`、`metrics`、`explain`、`kill_session`… | json 键（`model.Capabilities`） | 全局（驱动显示/隐藏） | model + 驱动 `Capabilities()` | `caps`（`app.js` applyEngineLayout） |
| Workspace 工作区 | 当前连接 + SQL 标签 + 布局 | 会话内 | 数据（主工作区） | ui2.js `ui2.tabs` | 前端内存（刷新不恢复） |
| SQLTab SQL 标签 | `id`、`name`、`sql`、`result`（活 DOM 节点引用） | `id` | 数据 | ui2.js | `ui2.tabs[]` |
| Object 对象 | `name`、`kind`（table/view/sequence/type/routine…）、`schema` | `schema.name` | 数据（对象树）/ 结构 | service 元数据 | `#tables` 树 + 搜索 `#obj-search` |
| TableData 表数据 | 行、列、主键、`pending` 待提交变更集 | 行主键 | 数据（结果/数据编辑） | service 变更集 | `#results` 网格 + `#pending-bar` |
| Execution 执行 | `sql`、结果块、`risk`(R0–R4)、`friction`(F0–F5)、`err` | `execution_id` | 数据 | service 执行门 | `#results` 的 `result-block`、`#risk`/`#friction` |
| ChangePlan 变更预案 | `pid`、DDL/DML、影响、回滚、审批指纹 | `pid` | 数据/结构 | service（服务端保管） | `dialog` + `approval` 卡片 |
| SchemaPlan 结构预案 | 列/索引/外键/约束/视图/序列/类型/例程 + DDL 预览 | pid | 结构 | service | `showSchema` 面板 |
| CompareResult 结构比对 | 差异（表/列/索引/约束/视图/物化视图）、升级/回退脚本 | 两侧连接 | 结构 | service | `renderCompare` |
| Monitor 监控快照 | metrics/slow-queries/locks/tables/history/health-check/alerts/sessions | 连接 + 采样时刻 | 监控 | service + 指标留存 | 面板容器 `#results`（`metrics-panel`/`health-panel`） |
| AgentConversation Agent 会话 | `id`、messages、approvals、steps | `id`（按连接隔离） | AI | service + agent | `showAgent` 面板 |
| Approval 审批 | `pid`、最终 SQL、影响、批准/拒绝 | pid | AI/数据 | service | `approvalCard` |
| Settings 设置 | theme/font_size/editor_tab_size/agent_*/api_key(只写不回显) | 单文件 JSON | 工具类 | `internal/infra/settings` | `ui2.settings`（`ui2.js`） |
| Token 令牌 | 服务模式 URL `#t=`；桌面模式 `window.DBRoverBootstrapToken` | 每启动重生成 | 工具类 | `internal/host`/`bootstrap.js` | `state.token`（`app.js`） |
| Backup/Migration 任务 | 源/目标、表、批次、校验摘要 | — | **运维任务（界面未接入）** | service/CLI | 无 UI 载体（`docs/12` §2.6 标未实现长任务形态） |

## 2. 实体关系

- `Connection 1—N Object`（按连接加载对象树；`loadTables`）。
- `Connection 1—N SQLTab`（工作区按连接；标签结果在前端内存）。
- `Object 1—1 TableData`（`browseTable/loadTablePage`）。
- `SQL 语句 1—N Execution结果块`（多语句 `renderExec` 每条一个 `result-block`，`exec_blocks_ui_test.go`）。
- `TableData N—M ChangePlan`（待提交变更集 → 预案 → apply → 回滚）。
- `Capability N—1 功能入口显示`（`hidden = !caps.xxx`，`capabilities_ui_test.go` 保证键名存在）。
- `Connection 1—1 AgentConversation`（会话按连接隔离，防跨连接复用已批预案）。
- `Connection 1—N Monitor快照`（每 5 秒增量，面板关闭即停，`metrics_ui_test.go` MutationObserver）。

## 3. UI 特有可视对象（重设计新增，语义化）

| 对象 | 用途 | 归属 | 是否改变业务规则 |
|---|---|---|---|
| ActivityRail 功能带 | 一级功能域导航容器 | 全局 | 否（纯导航） |
| DomainView 功能域视图 | 当前激活域（数据/结构/监控/AI/运维任务） | 全局 | 否 |
| `data-domain` / `data-action` | 给 AI Agent / 自动化脚本的稳定选择器 | 全局 | 否 |
| ContextHeader 当前区标题 | 显示激活域名 + 当前连接（去标语，靠布局让人看懂） | 全局 | 否 |

> 红线：以上可视对象不得修改 `service` 层状态机、风险分级、摩擦分级、审批门（`AGENTS.md:7`、`README.md` §1 四原则）。
