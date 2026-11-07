# 12 · 模块契约（UI 视角的数据生产者/消费者）

> 权威契约在 docs/12-api-contract.md 与 internal/api/openapi.json。本文件只列 UI 重设计**不得改变**的模块间依赖，避免重排破坏下游消费。

## 1. 不变契约（重设计红线）
- 界面与 Agent 共用 service 能力集（docs/02 C1、README §1）；UI 重排只改容器/导航，不改任何 /api/v1/* 路径、请求体字段、错误码。
- app.js 里声明的 36 条路由（含 /agent/*、/settings、/monitor/*、/kv/*、/compare、/exec、/export、/changes/:pid/apply）原样保留。
- action 字面量必须存在于 service/ddl.go（actions_ui_test.go）：create_table / create_sequence / create_type / create_trigger / add_check / add_foreign_key / drop_table / raw / create_view / create_routine / replace_routine / set_nullable / set_default / add_index / drop_index / add_column / drop_column / alter_column_type 等。
- capabilities.* 键名必须存在于 model.Capabilities（capabilities_ui_test.go）。
- 导出格式下拉必须与 service/export.go 的 ExportXxx 常量一致（export_ui_test.go）。

## 2. 模块依赖矩阵（谁产生、谁消费、时机、失败表现）
| 生产者 | 消费者 | 数据/字段 | 时机 | 失败时下游表现 |
|---|---|---|---|---|
| 连接管理（state.current） | 所有功能域 | id/engine/environment/access_mode/identity | 连接后 + 每次切域 | 未连接引导；身份变化重确认（E3） |
| 对象树 loadTables/renderObjects | 数据/结构域 | 对象名/kind/schema | 连接后、刷新后 | 搜索共用 renderObjects，隐藏不支持 key-value 搜索框（search_ui_test.go） |
| runSQL/renderExec | 数据编辑/标签/AI 上下文 | result-block 活 DOM、列/行、err | 执行后 | 每条独立块，失败块保留（E5） |
| attachEdit/pending | submitChanges | before 整行、三态 NULL/值/跳过 | 编辑时 | 内容不丢、冲突不覆盖（E9/E10） |
| plan/apply（服务端保管） | 结构/数据/审批 | pid/DDL/DML/回滚/指纹 | 预览→应用 | 指纹不匹配拒绝、审批拒绝可恢复 |
| 监控端点（6+） | 监控面板/AI | verdict/增量/锁链/巡检报告 | 面板打开时 5s 增量 | 超时降级、面板关闭 MutationObserver 停采（E12） |
| /agent/* | AI 域 | 会话/步骤/待批预案/导出 | 会话中 | 按连接隔离，越权被拒并审计 |
| /settings | ui2.applySettings | theme/font_size/tab_size/agent_*/api_key | 启动 + 保存后 | 401 不阻断、Key 不回显 |
| bootstrap.js / #t= | state.token | 令牌 | 启动 | #gate 令牌门（E1） |
| 备份/迁移 API/CLI（无 UI 生产者） | 运维任务域（空状态） | — | — | 界面未接入，如实标注（D6/E17） |

## 3. 重排不引入的新依赖
switchDomain 只读写纯 UI 状态（激活域、aria-current、二级导航展开），不调用后端、不改 state.current、不复制 #results 节点（保活 DOM 引用）。