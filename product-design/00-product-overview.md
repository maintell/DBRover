# 00 · 产品总览（UI 重设计基线）

> 状态：**待用户批准的设计基线，非已实现**。本轮（Improve/Mode B）只产出文档，不改业务代码。
> 证据来源均为 `F:\code\DBRover` 现有文件，行号以 HEAD `9c07928`、工作区干净为准。

## 1. 一句话

把 DBRover 的界面从「顶栏 15 个平铺按钮 + 永远可见的一切」重排成「**一个默认可启动的主工作区 + 按功能分组的导航**」，让高频核心闭环成为主角，次要/管理功能按任务进入、收敛分组、不喧宾夺主。

## 2. 产品与约束（取证）

| 事实 | 证据 |
|---|---|
| 以 AI Agent 为一等公民的跨引擎数据库工作台，Go 核心 + Web UI，桌面优先、可服务化 | `README.md:1-3`、`AGENTS.md:1` |
| UI 是免构建、内嵌的原生 JS 单页：`index.html` / `app.js`(4541 行) / `ui2.js`(340 行) / `style.css` / `bootstrap.js` | `internal/webui/webui.go:1` 的 `//go:embed`、`app.js`（实测 4541 行） |
| 两种形态加载同一份文件：`dbrover serve`（浏览器/团队）与 `dbrover-desktop`（Wails v2/WebView2，进程内 AssetServer.Handler，不监听端口） | `docs/superpowers/specs/2026-11-06-desktop-shell-and-ux-design.md`「形态对照」、`docs/adr/0017-desktop-shell-and-settings.md`、`ui2.js:14-21` |
| 长期正式前端是 React+TS+Vite，但本轮及 ADR-0017 明确复用免构建版本、零分叉 | `webui.go:3-5`、`README.md` §5 末、`docs/adr/0001-tech-stack.md` |
| 18 个含 UI 断言的 `*_test.go` 钉住了一批 id、动作名、能力位、文案、配色 | `internal/webui/*_test.go`（16 个 `*_ui_test.go` + `slowquery_test.go` + `irreversible_style_test.go`） |
| 文档驱动：docs 契约/ADR → 代码 → 测试 → 回填；文档与实现不一致视为缺陷 | `AGENTS.md:2` |
| `internal/ci` 文档一致性门禁只扫 `docs/` 目录，不扫 `product-design/` | `internal/ci/docs_test.go`（`docFiles`=`filepath.Join(root,"docs")`）、`sections_test.go`（`docsDir`） |

**边界结论**：`product-design/` 下新增纯 Markdown 交付物，不会触发 `TestDocTestCitationsExist` / `TestDocAdrLinksResolve` / `TestDocSectionRefsResolve`；`scripts/check.ps1`（gofmt/vet/build/test，见 `scripts/check.ps1`）只处理 Go，也不受影响。故本轮产物对现有门禁无破坏风险（详见 `19-implementation-plan.md`）。

## 3. 现状问题（取证）

`internal/webui/index.html:11-43` 顶栏把约 15 个按钮挤在 4 个 `tool-group`：
连接/新连接/刷新 ｜ AI/比对/体检/指标/会话 ｜ 新建表/新建视图/新建序列/新建类型/新建例程 ｜ 设置/退出。
其下是 SQL 标签栏、左侧对象树 `aside.sidebar`、右侧工作区（`#sql` textarea + `.editor-toolbar` + `#results`）。

三个结构性缺陷：
1. **无主次**：一切永远可见、平级。核心日常闭环（选连接 → 浏览对象 → 写/跑 SQL → 看结果 → 改数据）与「新建序列/类型/例程」「体检/会话/比对」抢占同一视线。
2. **无按功能分组**：分组只是视觉上的 `tool-group`，语义混杂（"AI" 和 "会话" 同组、"新建表" 和 "退出" 同栏）。
3. **新功能只能继续往顶栏塞**：`app.js` 已 4541 行、136 个顶层函数，顶栏已横向 `overflow-x: auto`（`style.css` `header.topbar`），每加一个能力就多挤一个按钮。

## 4. 本轮目标与非目标

**目标**
- 按「功能」重组信息架构，核心闭环成为默认落地页；次要/管理功能收敛分组、按任务进入。
- 产出可评审、可批准的完整设计基线 + 可落地实施方案 + 迁移映射。
- 遵守用户三条持久偏好：文案克制（删到看不懂就重做布局）、三体验齐升、不破坏现有功能。

**非目标（明确不做）**
- 不修改 `internal/webui/*.js` / `.html` / `.css` 业务代码（整份重写不可逆、需先确认设计；仓库文档先行）。
- 不切换到 React/TS/Vite，不引入构建步骤，不新增需 `//go:embed` 之外的资源文件。
- 不改名/删除任何被 18 条断言钉死的 id、动作名、能力位、文案、配色。
- 不把无 UI 入口的备份/恢复/迁移硬做成主角（见 `07` 决策 D6）。
- 不新增 ADR（本轮不改技术栈/进程模型/接口/安全边界/存储格式/依赖；若实施阶段决定调整这些，再按 `AGENTS.md:2` 补 ADR）。

## 5. 判定：主要功能（证据，不照抄建议）

| 功能 | 完成度/证据 | 频次定位 | 结论 |
|---|---|---|---|
| 连接管理 | `app.js` `loadConnections/selectConnection/newConnection`；`ui2.js` 连接抽屉 | 每次启动必用一次 | 主流程上下文（常驻为「当前连接」） |
| 对象树/表浏览 | `docs/01` FR-META-01..07；`app.js` `loadTables/renderObjects/searchObjects` | 每次任务 | **主流程** |
| SQL 编辑器/执行/结果网格 | `docs/05` 验收 14/15 通过；`app.js` `runSQL/renderExec/renderTable` | 每次任务 | **主流程（核心）** |
| 数据编辑（单元格/插删行/撤销） | `docs/06` 验收 12/12 全过；`app.js` `attachEdit/submitChanges/deleteRow/copyRow` | 高频 | **主流程（核心）** |
| 结构设计器（DDL 动作） | `docs/07` 10/12；`app.js` `showSchema/planXXX` | 按任务 | 次要域（分组进入） |
| 结构比对 | `app.js` `showCompare/runCompare`（`compare_ui_test.go`） | 低频 | 次要域 |
| 监控（指标/会话/体检/锁/慢查询/告警） | `docs/09` 8/12；`metrics_ui_test.go`/`sessions_ui_test.go` | 排障时 | 次要域 |
| AI Agent | `docs/11`；`app.js` `showAgent/askAgent`；M3 切片一、二完成 | 按需 | 次要域（一等公民但按任务进入） |
| 备份/恢复/迁移 | `docs/08`、`docs/12` §2.4 有 API/CLI；**`internal/webui` 全目录 grep 零 UI 入口**；`docs/12` §2.6 标注长任务形态「未实现」 | — | **界面未接入**（如实标注，留分组入口位，需拍板） |
| 设置/模式/令牌 | `ui2.js` 设置抽屉、`bootstrap.js` 令牌注入 | 偶发 | 工具类 |

**结论**：把「连接 + 对象树 + SQL 编辑器 + 结果/数据编辑」定为唯一一级主流程；「结构 / 比对 / 监控 / AI」按功能分组、通过明确入口进入；「运维任务（备份/恢复/迁移）」标注未接入；「设置、模式/令牌」归工具类。与任务里的「建议」一致，但由 `docs/05`、`docs/06` 验收完成度与 `app.js` 实际接线独立判定得出。

## 6. 关键决策（D1–D6）

- **D1 打开即主流程**：默认落地「数据」工作区，零点击可开始「选连接 → 浏览 → 写/跑 SQL → 看结果 → 改数据」。
- **D2 顶栏去按钮化**：15 个平铺按钮 → 只保留「当前连接选择 + 环境/身份状态 + 设置/退出」；其余按功能收进左侧功能带（Activity Rail）。
- **D3 按功能分组 + 渐进披露**：一级入口 ≤5（数据/结构/监控/AI/运维任务）；组内次级动作（新建序列/类型/例程、体检/会话等）二级展开，不常驻。
- **D4 上下文跨视图保留**：切换功能域时「当前连接」`state.current` 不变，不重复选连接、不重复录信息。
- **D5 不破断言**：所有被钉死的字符串原样保留，重排只改容器/父节点/CSS 布局/新增分组外壳与入口转发（逐项见 `18-migration-map.md`）。
- **D6 如实标注未接入能力**：备份/恢复/迁移在界面留「运维任务」分组位但标注「CLI/API 可用、界面未接入」，是否本轮补入口作为待拍板项，不伪装成主角。

## 7. 三体验齐升（可验证）

| 维度 | 现状 | 重设计后 | 证据/验收 |
|---|---|---|---|
| 用户 | 找功能靠扫 15 按钮 | 打开即主流程，功能按域收敛，≤5 一级入口，少点击 | `08`、`15` |
| 开发 | 新功能往顶栏塞、4541 行单文件 | 功能域对应固定容器，新增能力进对应组、不改顶栏 | `19` |
| AI Agent | 语义靠 id 猜 | 功能入口带 `data-domain`/`data-action`/`aria-label` + 稳定 `id`，可脚本化定位、报错明确 | `11`、`19` |
| 不破坏 | — | 18 条断言 + 两种形态 + check.ps1 全绿 | `17`、`18`、`19` |
