# 18 · 迁移映射（现有 id / 动作 / 按钮移到哪里、如何不破 18 断言与两种形态）

> 硬约束来源：internal/webui/ 下 16 个 *_ui_test.go + slowquery_test.go + irreversible_style_test.go = 18 个含 UI 断言的测试文件。它们用 strings.Contains 钉死精确字符串；**改名、删 id、改属性顺序、改文案、改配色都会让断言红**。下表逐项给出「原位置 → 新位置 → 保留什么」。

## 1. 总原则（不破的机理）
1. **只重排容器/父子关系，不重命任何 id**。app.js 用 $('id') / getElementById，ui2.js 用 u2('id')；只要 id 还在 DOM 里并能被命中，接线不断。
2. **index.html 里被整段钉死的按钮，保留整段字符串**（含 title 文本与 hidden 属性及顺序）：
   - id="btn-new-type" title="新建枚举类型" hidden （type_ui_test.go）
   - id="btn-new-routine" title="新建存储过程 / 函数" hidden （routine_ui_test.go）
   - 其余 id="btn-xxx" 形式（btn-metrics、btn-sessions、obj-search 等）保留 id 即可。
3. **app.js / ui2.js 的接线字符串一字不改**：所有 $('btn-xxx').onclick = ...、hidden = !caps.xxx、action: 'xxx'、capabilities.xxx、function xxx(、特定文案（「首次采样，暂无增量」「当前没有锁等待。」等）全部原样。
4. **style.css 被钉死项原样**：--danger: #f85149; 与 .card p.danger { color: var(--danger); }（irreversible_style_test.go）；不可逆提示 '<p class="danger">该变更不可逆' 保留。
5. **不重建 #sql / #results 节点身份**：ui2.js 包装 window.renderExec 并把 #results 活 DOM 节点引用存进 tab.result；重排若 replaceChildren 整个工作区会打断该包装。故域切换只切显示/二级导航，不重挂执行链路。

## 2. 顶栏按钮逐项迁移（index.html:11-43 → 新容器）
| 原 id | 断言钉死文件 | 新位置 | 必须保留 |
|---|---|---|---|
| btn-connections | （ui2.js wireUI2 接线） | ContextHeader 连接选择旁 / 连接抽屉入口 | id + ui2.js onclick 绑定 |
| btn-new-conn | （app.js boot 接线） | 连接抽屉 connections-new 旁（ui2.js 已转发 newConnection） | id + $('btn-new-conn').onclick = () => newConnection(); |
| btn-reload | （app.js boot） | 对象面板 .side-head（数据域） | id + $('btn-reload').onclick = () => loadTables(); |
| btn-agent | （app.js boot） | AI 域二级导航 | id + onclick = () => showAgent(); |
| btn-compare | （app.js boot） | 结构域二级「比对」 | id + onclick = () => showCompare(); |
| btn-health | metrics_ui_test（showHealth/导出） | 监控域二级「巡检」 | id + onclick = () => showHealth(); + showHealth 函数体 |
| btn-metrics | metrics_ui_test（hidden=!caps.metrics、id 在 index.html） | 监控域二级「性能」 | id="btn-metrics" + if ($('btn-metrics')) $('btn-metrics').hidden = !caps.metrics; |
| btn-sessions | sessions_ui_test（id、hidden=!caps.session_mgmt、onclick） | 监控域二级「会话」 | id + $('btn-sessions').hidden = !caps.session_mgmt + $('btn-sessions').onclick = () => showSessions() |
| btn-new-table | （app.js boot） | 结构域二级「新建」 | id + onclick = () => showCreateTable(); |
| btn-new-view | schema_ui_test（$('btn-new-view').onclick） | 结构域二级「新建」 | id + onclick 绑定 |
| btn-new-seq | schema_ui_test（btn-new-seq 出现、hidden=!caps.sequences） | 结构域二级「新建」 | id + if ($('btn-new-seq')) $('btn-new-seq').hidden = !caps.sequences; |
| btn-new-type | type_ui_test（整段 title+hidden、showCreateType、action: 'create_type'、type:{name,values}） | 结构域二级「新建」 | **整段 id/title/hidden 字符串** + showCreateType + onclick + create_type 请求体 |
| btn-new-routine | routine_ui_test（整段 title+hidden、showCreateRoutine、replace/create_routine、rt-replace、请求体字段） | 结构域二级「新建」 | **整段 id/title/hidden 字符串** + showCreateRoutine + onclick + replace_routine/create_routine |
| btn-settings | （ui2.js wireUI2） | ContextHeader 角落 | id + ui2.js openSettings 绑定 |
| btn-forget | （app.js boot） | ContextHeader 角落 | id + onclick |

## 3. 顶栏状态/上下文（不动 id，仅换容器）
#mode、#conn、#env、#identity、#profile、#settings-indicator → 全部移入 ContextHeader，id 与 ui2.js/app.js 写入逻辑不变（detectMode、applySettings、envBadge、loadIdentity）。

## 4. 工作区节点（保持身份，不重挂）
#tab-bar（ui2.renderTabs）、#sql、#btn-run（.primary）、#btn-run-all、#btn-explain、#btn-cancel、#run-continue、#risk、#friction、#cursor-pos、#status、#pending-bar、#table-toolbar、#results → 留在数据域 DomainView，父子关系不变。相关断言：exec_blocks_ui_test.go（result-block/renderTable(view,...)/失败块）、slowquery_test.go（SLOW_QUERY_SECONDS/PROMPT_SECONDS/finally 停表）、row_actions_ui_test.go（btn-insert-row 动态 id、删除本行/复制本行、三态 option）、longtext/json_editor/generated_column_ui_test.go。

## 5. 抽屉/弹窗（原 id 全保留，任意域可唤出）
#gate(#token/#btn-token)、#dialog(#dlg-title/#dlg-body/#dlg-ok/#dlg-cancel)、#connections-drawer(#connections-body/#connections-new/#connections-close)、#settings-drawer(#settings-body/#settings-save/#settings-close)。ui2.js 的 wireUI2/loadSettings/detectMode 与 app.js 的 dialog() 接线不动。

## 6. 动态 id（app.js 运行时生成，重排不碰）
sort-col/sort-dir/filter-*/page-size/btn-schema/export-format/btn-export/btn-insert-row/btn-bulk/btn-stop-bulk/btn-undo/ddl-preview/raw-ddl/raw-preview/raw-run/sch-droptable/metrics-panel/health-panel/btn-health-export/kv-*/cmp-left/cmp-right/bk-*/ct-*/sch-*/ix-*/fk-*/tr-*/ck-*/v-*/s-*/ty-*/rt-*/long-text/json-text 等：全部由现有函数生成，重排不改这些函数，故自动保持。

## 7. 动作/能力位/文案/配色（一字不改）
- 动作：create_table / create_sequence / create_type / create_trigger / add_check / add_foreign_key / drop_table / raw / create_view / create_routine / replace_routine / set_nullable / set_default / ...（actions_ui_test.go 对照 service/ddl.go）。
- 能力位：user_types / sequences / add_routine / session_mgmt / metrics / explain / materialized_views / add_check / add_trigger / add_foreign_key / kill_session / lock_graph / slow_query_stats / capacity / ...（capabilities_ui_test.go 对照 model.Capabilities）。
- 文案：终止会话、删除本行、复制本行、这些列是 NOT NULL，不能写 NULL、JSON 不合法：（内容还在，重新打开继续改）、生成列：由引擎按表达式计算，不能直接改、首次采样，暂无增量、当前没有锁等待。、没有开启指标留存。、需要更多采样、视图 / 物化视图差异、（物化视图）、不可逆提示。
- 配色：--danger: #f85149; + .card p.danger（irreversible_style_test.go）。

## 8. 两种形态不破（serve / desktop）
- webui.go 的 //go:embed 列表不变（仍是 index.html app.js ui2.js style.css bootstrap.js）；不新增需内嵌的资源文件、不引外部 CDN/字体（桌面进程内 AssetServer 无网络假设）。
- bootstrap.js 仍是占位；桌面 window.DBRoverBootstrapToken 注入链路与团队 #t= 令牌门不变（internal/ci/desktop_test.go 守住）。
- ui2.js detectMode 写 #mode「桌面/服务」不变；一份文件零分叉（ADR-0017）。
- 不引入构建步骤、不切 React（跳过 Vercel/React 系列技能）；不新增 go.mod 依赖。

## 9. 新增内容（不触碰任何被钉死字符串）
ActivityRail 5 个入口（data-domain/data-action/aria-label/aria-current）、ContextHeader/DomainView/.domain-title/.secondary-nav/.ops-empty 外壳、style.css 追加层里的 .context-header/.activity-rail/.rail-item/.domain-view 等新类与 --rail-w/--space-*/--fs-*/--z-rail/--focus 令牌。均为新增，不覆盖既有选择器。

## 10. 验证映射（实施阶段逐条跑）
go test ./internal/webui/... → 18 个文件全绿；go test ./internal/ci/... → 文档/桌面/标签/发版门禁全绿；pwsh -File scripts/check.ps1 → gofmt/vet/build/test 全绿。见 19-implementation-plan.md。