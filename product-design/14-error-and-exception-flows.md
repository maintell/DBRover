# 14 · 异常流与恢复路径

> business-workflow-ux 第 6/11 步要求：每类异常有「检测点 + 用户可见反馈 + 恢复路径 + 终态」。证据锚定现有实现，重设计不得削弱这些恢复能力。

| # | 异常 | 检测点 | 用户反馈 | 恢复路径 | 终态 | 证据 |
|---|---|---|---|---|---|---|
| E1 | 令牌缺失/失效 | `readHashToken` / 401 | `#gate` 令牌门 + 说明 | 输入 `#t=` 令牌 / 桌面注入量 | 进入 / 保持门 | `app.js`、`index.html:85-95`、`ui2.js` `loadSettings` 401 不阻断 |
| E2 | 连接失败 | `testConn` / `loadConnections` | `conn-test.err`「不可用：…」/ notice | 测试/重连/编辑连接（口令重输） | 可用 / 删除 | `ui2.js` `testConn/delConn` |
| E3 | 连错库（身份指纹变化） | `loadIdentity` | `#identity` 徽标 + 重新确认环境 | 重新声明环境 | 已确认 | `docs/01` FR-ENV-06/07 |
| E4 | 引擎不支持某能力 | `applyEngineLayout` 能力位 | 入口 `hidden`（不摆必然失败的按钮） | 换连接/换引擎 | 无该入口 | `type_ui_test.go`/`capabilities_ui_test.go` |
| E5 | 多语句某条失败 | `renderExec` 逐块 | 「第 N 条 · 失败：err」+ `遇错继续` | 点标题 `jumpToStatement` 回编辑器改 / 重跑 | 失败块保留 | `exec_blocks_ui_test.go`、`docs/05` 验收 4 |
| E6 | 长查询 | `startQueryTimer` | 状态栏「（可取消）」+ 30 秒弹窗 | 继续等 / `cancelRunning` | 完成或取消 | `slowquery_test.go` |
| E7 | 取消/超时 | 引擎侧 | 已取消 + 停止计时 | 回编辑器重跑 | 编辑器就绪 | `docs/05` 验收 9 |
| E8 | 只读连接尝试写 | service 执行门 + UI 入口隐藏 | 拦截提示 / 不摆行级入口 | 切到读写连接（放宽需显式+审计） | 拒绝 | `row_actions_ui_test.go`、`AGENTS.md:7` |
| E9 | 乐观并发冲突 | `submitChanges` WHERE 原值 | 冲突提示「不覆盖」 | 回待提交，内容不丢，重新读后改 | 待提交 | `docs/06` FR-DATA-05 |
| E10 | 校验失败（NULL/JSON/NOT NULL/longtext） | 界面拦截 / 编辑器 | 「这些列是 NOT NULL…」/「JSON 不合法：（内容还在…）」/ 长文本全文编辑器 | 就地修正，输入不丢 | 可提交 / 可重试 | `row_actions_ui_test.go`、`json_editor_ui_test.go`、`longtext_ui_test.go` |
| E11 | 不可逆变更 | `planDropColumn/drop_table` | `<p class="danger">该变更不可逆`（红，非 warn） | 确认 / 取消 / 用回滚 SQL | 应用或放弃 | `irreversible_style_test.go`（`--danger:#f85149` + `.card p.danger` 被钉死） |
| E12 | 监控采集超时/无权 | 3s 超时 / 降级 | 「首次采样，暂无增量」「没有开启指标留存。」「当前没有锁等待。」 | 明确降级说明，不假装精确 | 降级只读 | `metrics_ui_test.go`、`docs/09` §2 |
| E13 | Kill 会话不可逆 | `killSession` | `dialog('终止会话'` 带 SQL 摘要 | 确认/取消；只读不给入口 | 已终止/放弃 | `sessions_ui_test.go` |
| E14 | 结构比对物化视图混淆 | `renderCompare` | 「（物化视图）」「视图 / 物化视图差异」标注 | 人工 review，不自动出脚本 | need_review | `compare_ui_test.go`、`ADR-0005` |
| E15 | 导出格式后端不支持界面没入口 | `TestExportFormatsOfferedMatchBackend` | 门禁变红（防「只能敲 curl」） | 补下拉项 | 下拉与后端一致 | `export_ui_test.go` |
| E16 | 设置保存失败/Key 不回显 | `saveSettings` | 「保存失败：…」/ 已配置占位 | 重试；Key 只进凭据库 | 已保存/保持 | `ui2.js`、`ADR-0017` |
| E17 | 备份/恢复/迁移界面无入口 | 断裂检查 | **如实标注「CLI/API 可用、界面未接入」空状态** | 去 CLI/`docs/08`；或拍板补界面 | 未接入（已知限制） | `docs/12` §2.6、`17-ux-review-report.md` 断裂#5 FAIL |
| E18 | 无法退出/撤销的流程 | 全链路 | 每个弹窗有取消/关闭；`pending` 可丢弃；标签可关 | 恢复路径见上 | 可退出 | `ui2.js` `closeTab/dlg-cancel`、`index.html` 抽屉 close |

**终态约束**：不存在「提交中/审批中/执行中」无法退出或撤销的状态；`finally` 停表、`cancelRunning`、`discard`、`dlg-cancel` 提供退出通道。
