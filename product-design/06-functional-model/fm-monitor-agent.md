# FM-09~12 · 监控域 + AI 域（次要，按任务进入）

## FM-09~11 监控
- **目的/角色/前置**：排障；工程师/DBA/Agent；已连接 + `caps.metrics/session_mgmt/lock_graph/slow_query_stats/capacity` 等。
- **入口**：`ActivityRail` →「监控」→ 二级（性能/会话/巡检）。当前连接继承。
- **流程**：`btn-metrics`→`showMetrics(false)` 六块 `Promise.allSettled` 并发 → `renderMetrics` + 5 秒增量 → 关闭面板 `MutationObserver` 停采集；`btn-sessions`→`showSessions`→`killSession`（需确认）；`btn-health`→`showHealth`→`btn-health-export`。
- **分支/异常**：E12 降级「首次采样，暂无增量」「没有开启指标留存。」「当前没有锁等待。」；E13 Kill 确认 + SQL 摘要；只读不给 Kill。
- **组件/操作**：`#results` 内 `metrics-panel`/`health-panel`（动态 id 保留）、`showMetrics/renderMetrics/slowSection/alertSection/historySection/lockSection/sizeSection/showHealth/showSessions/killSession`。
- **数据/状态/下游**：只读采集（3s 超时、LIMIT），结论来自 `out.verdict` 不二次判断；面板关闭即停（`metrics_ui_test.go`）；结果不写库，供 AI 上下文与排障。
- **权限/验收**：`hidden=!caps.metrics`、`hidden=!caps.session_mgmt`、`!!caps.kill_session && access_mode!=='read-only'`（`metrics_ui_test.go`/`sessions_ui_test.go`）；巡检可导出 markdown。

## FM-12 AI Agent
- **目的/角色/前置**：会话式完成数据库任务；工程师/Agent；已连接 + 模型服务配置（设置里的 `agent_base_url/agent_model`），会话按连接隔离。
- **入口**：`ActivityRail` →「AI」→ `btn-agent`→`showAgent`。
- **流程**：`askAgent` → 步骤时间线 `renderAgent`（思考/工具/参数/结果摘要/耗时可展开）→ 待批 `approvalCard`（最终 SQL 比「谁提的」更显眼）→ 批准/拒绝 → 结论附证据 → `exportAgent`。
- **分支/异常**：R2+ 审批门、R3/R4 二次确认、提示注入防御、`envAllowsAutonomous` 环境上限、`ReadOnlyQuery` service 侧只读判定；审批失败可重试/拒绝，不静默执行。
- **组件/操作**：`agent-q/agent-mode`、`showAgent/askAgent/renderAgent/approvalCard/exportAgent/sqlOfArgs`、审批 `yes/no.onclick`。
- **数据/状态/下游**：会话/消息/审批经 `/agent/*`；写操作 100% 审批 + 审计；工具与人工共用 `service`（`docs/11` §1）。
- **权限/验收**：三级权限（只读/写入需批准/完全授权）× 环境上限；`docs/01` FR-AGT-01..08、M3 评测集。

## 重排约束（两域共同）
- 「AI/比对/体检/指标/会话」这些原本挤在顶栏的按钮，收进对应功能域的二级导航；但 `id`、`onclick` 绑定、能力位判断、文案字符串**原样保留**（`18-migration-map.md`）。
- 「新建序列/类型/例程」进结构域二级；「体检/会话/指标」进监控域二级，不再与「AI」平铺同组。
