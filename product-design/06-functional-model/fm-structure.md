# FM-06~08 · 结构域（次要，按任务进入）

## 1. 目的 / 2. 角色 / 3. 前置
改结构、创建对象、比对两库；工程师/DBA/Agent（变更预案）；需已连接 + 对应 `capabilities` + 非只读（写动作）。

## 4. 入口/导航
`ActivityRail` →「结构」(`data-domain="structure"`)。进入后二级导航：设计器 / 新建 / 比对。当前连接从 `ContextHeader` 继承，不重选。

## 5. 流程
- 主（设计器）：选对象 → `showSchema/refreshSchema/renderSchema` → 列/索引/外键/约束/分区区 → `planXXX` → `ddl-preview` → `confirmPlan` → `applyPlan` → `refreshSchema`。
- 主（新建）：`btn-new-table`/`btn-new-view`/`btn-new-seq`/`btn-new-type`/`btn-new-routine` → 对应 `showCreateXXX` → 表单 → `action: 'create_*'` → 预案 → 应用。
- 主（比对）：`btn-compare` → `showCompare` 选两侧 → `runCompare` → `renderCompare` 差异清单（物化视图标注）→ 升级/回退脚本（只读）。
- 分支/异常：E4（能力位隐藏 `hidden=!caps.xxx`）、E11（不可逆 `danger` 红 + 回滚）、E14（物化视图 need_review）、乐观并发/依赖阻断（`docs/07`）。

## 6. 页面/组件
复用现有 `dialog`（`#dlg-title/#dlg-body/#dlg-ok/#dlg-cancel`）、schema 面板、DDL 预览、比对结果区；这些 id 全部保留，重排只改「由哪个入口打开」与容器归属。

## 7. 操作
`showCreateTable/showCreateView/showCreateSequence/showCreateType/showCreateRoutine`、`showSchema/planAddColumn/planDropColumn/planColumnChange/planAddCheck/planCreateIndex/planAddForeignKey/planAddTrigger/planRawDDL`、`showCompare/runCompare/renderCompare`。

## 8. 数据变化
不直接改库；走 `plan` → `apply`（服务端保管预案、指纹绑定、审计）。`action` 字面量必须存在于 `service/ddl.go`（`actions_ui_test.go`）。

## 9. 状态变化
`13-state-machines.md` §3 域切换 + `03-business-lifecycle.md` §2 结构生命周期。

## 10. 下游链接
结构变更 → 对象树刷新 `loadTables`、结果区可重跑；比对结果 → 升级脚本（人工 review，不自动执行）。

## 11. 权限
能力位 `user_types/sequences/add_routine/materialized_views/add_check/add_trigger/add_foreign_key` 决定子项显隐（`capabilities_ui_test.go` 保证键名存在）；只读连接不给写入口。

## 12. 验收
- 新建类型/例程按钮默认 `hidden` 且 `hidden=!caps.xxx`（`type_ui_test.go`/`routine_ui_test.go`）。
- 列级 `set_nullable/set_default`、分区区四字段、物化视图标注齐全（`schema_ui_test.go`/`partitions_ui_test.go`/`compare_ui_test.go`）。
- 不可逆提示为 `.danger` 红（`irreversible_style_test.go`）。
- 「删完看不懂就重做布局」：二级导航靠图标+`data-action`+`aria-label`，不加回标语。
