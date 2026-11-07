# 08 · 导航模型

## 1. 导航层级
- 一级：`ActivityRail`（数据 / 结构 / 监控 / AI / 运维任务）——固定左侧，图标+`aria-label`+`data-domain`。
- 二级：当前域内的动作组（如结构域「设计器 / 新建 / 比对」），默认折叠或面板内分组，不占用顶栏。
- 处理页/结果页：全部在 `DomainView` 内通过 `#results`/`dialog`/`drawer` 呈现（沿用现有「入口—处理—结果」三件套，见 `09-page-specifications`）。

## 2. 合法转换（无孤岛/无法到达页面）
`switchDomain(target)`：`data ↔ structure ↔ monitor ↔ agent ↔ ops` 任意可达；`utility` 抽屉（连接/设置/令牌门）可由任意域唤出并「关闭/取消」返回原域（`state.current` 保留）。`13-state-machines.md` §3 给出守卫。

## 3. 返回/取消/深链
- 每个 `drawer` 有 `#connections-close`/`#settings-close`，每个 `dialog` 有 `#dlg-cancel`；`gate` 有进入返回。
- 多语句结果块标题 `jumpToStatement` 跳回编辑器对应语句；结果可「清空结果」`tab-clear`。
- 深链：`#t=` 令牌（团队）、`window.DBRoverBootstrapToken`（桌面）；不改变现有 token 语义。工作区/标签结果刷新不恢复是已知设计（`ui2.js` 注释），不伪装成深链。

## 4. 跨页上下文传递（不重复录入）
| 上下文 | 传递方式 | 丢失兜底 |
|---|---|---|
| 当前连接 `state.current` | 全局 state（跨域共享） | `ContextHeader` 常驻显示，切换域不重选 |
| SQL 标签 `ui2.tabs` | 前端内存 `ui2` 闭包 | 刷新不恢复，空标签显示「执行以查看结果」 |
| 选中对象 `state.tableName` | 全局 state | 结构域进入时复用，无选中则提示先选对象 |
| 待提交 `pending` | 全局 state | 关闭标签/切域前保留，丢弃需确认 |
| 筛选/排序 `#table-toolbar` | DOM 状态 | 重跑即按当前条件 |
| 监控面板开关 | `metricsTimer` + MutationObserver | 面板消失即停采，切回重新采样 |

## 5. 渐进披露与「聚焦」
- 默认只暴露 `data` 域全貌；其余域点一级入口后才展开二级动作。
- 能力位 `hidden=!caps.xxx` 继续决定子项显隐：连到不支持的引擎时，结构/监控域里对应项不出现，而不是摆一个点了报错的按钮（`type_ui_test.go` 注释）。
- 「体检/指标/会话」默认 `hidden`（`index.html` 已如此），重排后仍由能力位点亮，不在顶栏抢视线。

## 6. 导航语义化（AI Agent 体验）
每个入口带 `data-domain`、`data-action`、`aria-label` 与稳定 `id`，使自动化/接入的 Agent 能用语义选择器定位，而非靠按钮文字或坐标；配合 `app.js` 已有「明确错误文案」保持报错可诊断。
