# WF-01 · 日常查数主流程（一级主流程，端到端验收基线）

> business-workflow-ux 第 6/13 步：主/分支/异常 + 跨模块端到端串接。

## 主流程（编号即预期交互步数）
1. 启动 → 默认 domain=data；ContextHeader 显示上次/默认连接（未连接则引导）。
2. 选/新建连接 → selectConnection → state.current、能力位、#env/#identity/#profile、对象树 loadTables。
3. 对象树 #tables（或 #obj-search 搜索）点表 → browseTable/loadTablePage 出可编辑网格；或直接在 #sql 写查询。
4. btn-run（Ctrl+Enter）→ runSQL → 执行中（计时/可取消/风险 #risk、摩擦 #friction 徽标）。
5. renderExec → #results 出 result-block 网格（多语句每条一块）。
6. 看结果 / 改数据：attachEdit → #pending-bar 待提交 → submitChanges → 预案预览（前后对比 + 回滚 SQL + R3/R4 二次确认）→ apply → 刷新 + 成功反馈 + 可一键撤销。

**完成条件（可验证断言）**：结果已读且（若为写）变更被 service 持久化 + 审计 + 下游对象树/结构可读到 + 用户看到明确成功/失败反馈。

## 分支
- 多语句：btn-run-all、遇错继续（#run-continue）、点标题 jumpToStatement。
- 只读结果：btn-explain 计划、exportTable 导出（格式与后端一致，export_ui_test.go）。
- 标签：addTab/switchTab/closeTab，各自结果恢复（internal/ci/tab_results_test.go）。
- 大字段：longtext 全文编辑器 / JSON 编辑器（不截断写回）。

## 异常（详见 14）
E1 令牌、E2 连接、E4 能力位、E5 多语句失败、E6/E7 长查询/取消、E8 只读、E9 并发冲突、E10 校验、E11 不可逆、E15 导出格式、E18 可退出。

## 跨模块端到端（起点 → 终点 → 下游消费 → 异常注入 → 恢复）
未连接 → 已连接 → 对象选定 → 有结果 → 已提交(变更) → 对象树/结构刷新(下游) → 注入: 并发冲突/长查询/不可逆 → 恢复: 回待提交/取消/取消审批 → 终态可退出。
断裂检查 10 条逐项结论见 17-ux-review-report.md。