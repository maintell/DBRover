# FM-13~17 · 运维任务 + 工具类

## FM-13 备份/恢复/迁移（界面未接入，如实标注）
- **目的**：备份产物、恢复到新库、跨引擎迁移校验。
- **现状证据**：`docs/08`、`docs/12` §2.4 有 API/CLI；`internal/webui` 全目录 grep 对 `backup/restore/migrate` 零命中（仅 `compare` 一处相关）；`docs/12` §2.6 明确「长任务化未实现：无 tasks 表/进度/暂停续跑」。
- **入口（重设计后）**：`ActivityRail` →「运维任务」(`data-domain="ops"`)，**显示空状态**「备份 / 恢复 / 迁移：CLI 与 API 可用，界面暂未接入」+ 指向 `docs/08` 与 CLI 用法的指引，不摆点了必然失败的按钮。
- **下游/权限/验收**：断裂检查第 5 条当前 FAIL（`17-ux-review-report.md`）；是否本轮补界面入口作为待拍板项 D6。用户拍板前**不得**把它做成主角或伪造入口。
- **注意**：不新增 ADR；若实施阶段决定补界面并引入长任务模型（tasks 表/进度/暂停续跑），那属于「跨模块接口/存储格式」变更，须按 `AGENTS.md:2` 先补 ADR 与 `docs/12` 契约。

## FM-14 连接管理（工具）
`btn-connections` → `connections-drawer`（`#connections-body/#connections-new/#connections-close`）→ `renderConnections/connCard/switchConn/testConn/delConn`。删除前 `dialog` 确认「正在跑的查询会中断」。常驻但置于 `ContextHeader` 连接选择旁，不作主角。

## FM-15 设置（工具）
`btn-settings` → `settings-drawer` → `openSettings/settingsForm/saveSettings/applySettings`。深/浅 `theme`、`font_size`、`editor_tab_size`、模型服务 `agent_base_url/agent_model/agent_context_tokens`、API Key（只进 OS 凭据库、不回显）、快捷键说明。Ctrl+S 保存。置于顶栏角落。

## FM-16 令牌/模式（工具）
`bootstrap.js`（桌面 `window.DBRoverBootstrapToken`）/ `#gate`（团队 `#t=`）；`#mode` 徽标「桌面/服务」由 `detectMode` 写入；`#settings-indicator` 点表示模型服务已配置。注入链路零分叉（`internal/ci/desktop_test.go`）。

## FM-17 退出（工具）
`btn-forget` → `forgetToken`（清本页令牌）。置于设置旁，非主角。

## 重排后「工具类」布局
顶栏只保留：品牌 + 当前连接选择 + 环境/身份/配置指示徽标 + 设置 + 退出。原「连接/新连接/刷新」收进连接抽屉与对象面板（`btn-reload` 保留在对象面板头部，`btn-new-conn` 保留在连接抽屉 `connections-new` 与 `ui2.js` 的转发）。
