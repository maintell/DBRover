# FM-01~05 · 数据工作区（一级主流程，主角）

## 0. 定位
打开应用默认落地的功能域 `data-domain="data"`，用户零点击即可开始「选连接 → 浏览对象 → 写/跑 SQL → 看结果 → 改数据」。这是重设计后唯一常驻主区。

## 1. 目的
让日常最高频闭环在一个视图内走完，结果被下游（结构/数据编辑/审批）正确消费。

## 2. 角色
工程师（主）、只读用户（只读降级）、AI Agent（经工具面只读/审批）。

## 3. 前置条件
令牌就绪（`#gate` 已过）、`state.current` 已连接、能力位 `applyEngineLayout` 已加载。

## 4. 入口/导航
- 应用启动即进入（默认 `domain=data`）。
- 左侧 `ActivityRail` 第一项「数据」；切走后可一键返回。
- 当前连接选择常驻 `ContextHeader`（不因切域重选）。

## 5. 流程
- 主：选连接 → 对象树 `loadTables/renderObjects` 或搜索 `#obj-search` → 写 `#sql`（或点表 `browseTable/loadTablePage`）→ `runSQL` → `renderExec` 出 `result-block` / 可编辑网格 → `attachEdit` 改 → `submitChanges` → 预案预览 → `apply` → 结果刷新 + 审计。
- 分支：多语句 `遇错继续`、`btn-run-all`、`btn-explain` 计划、切标签 `ui2.tabs`、结构化筛选 `#table-toolbar`、导出 `exportTable`。
- 异常：E1/E4/E5/E6/E7/E8/E9/E10/E15（见 `14-error-and-exception-flows.md`），每类有反馈 + 恢复 + 终态。

## 6. 页面/组件（布局重排后）
- `ContextHeader`：当前连接 `#conn` + `#env` + `#identity` + `#profile`（去标语，靠布局让人看懂）。
- `aside.sidebar` 对象面板：`#obj-search` + `#tables` + `#table-count`（保持 id）。
- `nav#tab-bar` SQL 标签栏（`ui2.js` renderTabs）。
- `.editor-wrap`：`#sql` + `.editor-toolbar`（`#btn-run` 主按钮、#btn-run-all / #btn-explain / #btn-cancel / #run-continue / #risk / #friction / #cursor-pos / #status`）。
- `#pending-bar`、`#table-toolbar`、`#results`。
- 各 `.overlay`/`.drawer`（gate/dialog/connections/settings）保持原 id。

**重排动作**：仅把这些容器从「顶栏按钮驱动」改为「功能域外壳 + 左功能带驱动」；`#sql`/`#results` 节点身份不变。

## 7. 操作（全部保留原 id/函数名）
`selectConnection`/`loadConnections`/`newConnection` · `loadTables`/`renderObjects`/`searchObjects`/`browseTable`/`loadTablePage` · `runSQL`/`runSQL(false,true)`/`explainSQL`/`cancelRunning`/`renderExec` · `attachEdit`/`showInsertRow`/`deleteRow`/`copyRow`/`submitChanges`/`exportTable` · `ui2.tabs`（addTab/switchTab/closeTab/restoreTabResult）。

## 8. 数据变化
只读查询不改库；写经 `service` 执行门 → 库内变更 + 审计 + 回滚预案。结果集/标签结果仅前端内存（刷新不恢复）。

## 9. 状态变化
见 `13-state-machines.md` §2（编辑器/执行/变更集状态机）。

## 10. 下游链接
- 对象 → 结构域 `showSchema`（复用选中对象，不重选）。
- 结果 `#results` → 数据编辑 `pending` → 预案 `dialog/approval`。
- 执行结果 → 监控/AI 的「真实结果行」上下文（`docs/11` FR-AGT-11）。
- 连接 `state.current` → 所有功能域共享（跨页上下文保留）。

## 11. 权限
只读连接：无行级入口、写被双层拦截（`row_actions_ui_test.go`）；能力位决定 `btn-explain` 等显隐。

## 12. 验收
- 打开应用到「跑出一条 SELECT 结果」≤2 次交互；到「改一格并提交」全程不重选连接。
- 多语句每句一个 `result-block`、点标题跳回语句（`exec_blocks_ui_test.go`）。
- 切标签结果各自恢复（`internal/ci/tab_results_test.go`）。
- 18 条断言全绿（`17-ux-review-report.md`）。
