# WF-02 · 结构变更（次要域，按任务进入）
1. 从 data 域点「结构」→ switchDomain('structure')（state.current 不变，复用已选对象）。
2. 选对象 → showSchema/refreshSchema → 列/索引/外键/约束/分区区（partitions_ui_test.go 四字段）。
3. 填变更 → planAddColumn / planDropColumn / planColumnChange('set_nullable'|'set_default') / planCreateIndex / planAddForeignKey / planAddCheck / planAddTrigger / planRawDDL。
4. ddl-preview 预览 → confirmPlan（不可逆 .danger 红 + 回滚 SQL，FR-SEC-04）→ applyPlan。
5. refreshSchema → 对象树 loadTables 刷新（下游消费）→ 明确成功反馈；失败则冲突/依赖阻断提示，内容不丢。
分支：新建表/视图/序列/类型/例程（能力位 hidden=!caps.xxx）；showCompare/runCompare/renderCompare 结构比对（只读，物化视图标注，不自动出脚本）。异常：E4/E9/E11/E14。

# WF-03 · 监控与排障
data/任意域 → 监控 → 性能(六块并发+首屏结论)/会话(Kill 需确认)/巡检(可导出) → 面板关闭即停采集 → 返回原域。降级 E12、Kill E13。

# WF-04 · AI Agent 任务
任意域 → AI → askAgent(按连接隔离会话) → 步骤时间线 → 待批 approvalCard(最终 SQL) → 批准/拒绝 → 结论附证据 → exportAgent。R2+ 审批、R3/R4 二次确认、envAllowsAutonomous、ReadOnlyQuery。

# WF-05 · 运维任务（界面未接入，如实标注）
任意域 → 运维任务 → 空状态「CLI/API 可用、界面暂未接入」+ docs/08/CLI 指引 → 退出/返回。当前终点是「已知限制」，不是业务完成；是否补入口为 D6 待拍板。