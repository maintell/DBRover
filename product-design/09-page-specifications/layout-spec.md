# 09 · 页面/布局规格（重排后的结构，11 项）

> 不是静态线框：以下是可落地的 DOM 容器规格、状态与操作。id 全部沿用现有值（18-migration-map.md）。

## 全局外壳 #app（goal/roles/entry/layout/data/states/ops/feedback/transitions/empty-error/permission）

| # | 项 | 规格 |
|---|---|---|
| 1 | 目标 | 让主流程成为默认可见主角，次要功能按域进入 |
| 2 | 角色 | 全部角色；权限由 service 判定，UI 只做显隐 |
| 3 | 入口 | 应用启动 → domain=data |
| 4 | 布局 | ContextHeader(顶) + ActivityRail(左一级) + DomainView(右，含 aside.sidebar / #tab-bar / .editor-wrap / #results)；#app 用 flex 列、main 用 flex 行（沿用 style.css #app/main），新增一层 rail 容器 |
| 5 | 数据源 | state.current、ui2.tabs、caps、#results 活 DOM |
| 6 | 状态 | domain=data 或 structure 或 monitor 或 agent 或 ops；未连接/令牌待验；面板开/关 |
| 7 | 操作 | switchDomain、连接选择、设置/退出、各域动作 |
| 8 | 反馈 | #status、#risk/#friction 徽标、notice、result-block、approval 卡片 |
| 9 | 转换 | 见 08-navigation-model.md §2、13-state-machines.md §3 |
| 10 | 空/错 | 未连接引导；空标签「执行以查看结果」；ops 域「界面未接入」空状态；E1–E18（14） |
| 11 | 权限 | 只读/能力位/环境守卫（02） |

## ContextHeader（顶栏重排后）
- 保留：.brand、#mode、#conn(select)、#env、#identity、#profile、#settings-indicator、#btn-settings、#btn-forget。
- 移出（进对应容器）：btn-connections / btn-new-conn / btn-reload / btn-agent / btn-compare / btn-health / btn-metrics / btn-sessions / btn-new-*（见 18）。
- 去标语：title 长说明从按钮移到 aria-label；靠 #env/#identity/#profile 徽标 + 区标题让人看懂，不加回说明文字。

## ActivityRail（新增容器，一级功能带）
- 5 个入口：数据 / 结构 / 监控 / AI / 运维任务；data-domain、data-action=switch-domain、aria-label、aria-current。
- 窄屏（复用 @media max-width:900px 段）折叠为可唤出抽屉，不重写整套 CSS。

## 数据域 DomainView（主角，保持原有节点身份）
- aside.sidebar：.side-head + #obj-search + #tables + #table-count；btn-reload 置头部（原顶栏刷新迁移至此）。
- nav#tab-bar：ui2.js renderTabs 产物，不动。
- .editor-wrap：#sql + .editor-toolbar（主按钮 #btn-run 视觉强化为 .primary，其余 #btn-run-all / #btn-explain / #btn-cancel / #run-continue / #risk / #friction / #cursor-pos / #status 原位）。
- #pending-bar、#table-toolbar、#results：原样。
- 强调「主要」的手段：#btn-run 主色 + 更大点击区 + 编辑器/结果区占最大面积（Data-Dense Dashboard：最大数据可见）；次要按钮降级为图标/次级色，不删除。

## 结构/监控/AI/运维任务 DomainView
- 复用现有面板（schema/compare/metrics/sessions/health/agent）挂到当前域的二级导航下；进入时不重建 #sql/#results 节点身份（保 ui2.js 活 DOM 与 renderExec 包装）。
- ops 域：空状态卡片「CLI/API 可用、界面暂未接入」+ docs/08 / CLI 指引，不摆假按钮。

## 抽屉/弹窗（原 id 全保留）
#gate、#dialog、#connections-drawer、#settings-drawer 及内部 #dlg-* / #connections-* / #settings-* 保持；任意域可唤出，关闭返回原域。