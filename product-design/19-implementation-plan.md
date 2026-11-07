# 19 · 具体实施方案（获用户批准后才执行；本轮只交付方案，不改代码）

> 仓库文档先行（AGENTS.md:2）：先有被批准的设计基线（本 product-design/），再改代码 → 测试 → 文档回填。整份 UI 重排不可逆、且改变方向，故本轮停在「方案」，等用户拍板（见文末与 17 报告）。

## 0. 前置：需要用户拍板后才能动代码
实施会改 index.html / style.css（及少量 app.js/ui2.js 的容器接线）。按 Mode B 边界与用户「整份重排需先确认设计」的要求，本轮**不修改任何业务代码**。获批准后按下列顺序执行；若用户要求先验证再定，也先做第 1 步的「静态接线自检」而非直接重写。

## 1. 改哪些文件（严格限定在 //go:embed 的 5 个文件内，不新增资源）
| 文件 | 改动性质 | 是否可破断言 |
|---|---|---|
| index.html | **结构重排**：顶栏去按钮化 → ContextHeader；新增 ActivityRail 容器与 DomainView 外壳；把 15 个按钮按域移进对应容器；保留全部 id 与两段精确 title+hidden 字符串 | 高风险（精确字符串），按 18 逐项核对 |
| style.css | **追加层**：在 v2 段后追加 .context-header/.activity-rail/.rail-item/.domain-view/.domain-title/.secondary-nav/.ops-empty 与 --rail-w/--space-*/--fs-*/--z-rail/--focus；不动 --danger 与 .card p.danger；扩展 @media max-width:900px | 低（追加，不覆盖） |
| ui2.js | **最小追加**：新增 switchDomain(target) 与 ActivityRail 点击接线（data-domain/data-action），在 wireUI2 里补一级入口绑定；不动 renderExec 包装、renderTabs、settings/connections 逻辑 | 中（追加函数，不改既有字符串） |
| app.js | **理想情况下零改动**：所有 $('id') 接线、动作、能力位、渲染函数一字不改。仅当某按钮的 onclick 在 boot() 里依赖「元素已在顶栏」而需迁移绑定位置时，才做等价搬移（不改字符串本身） | 高（默认不碰） |
| bootstrap.js / webui.go | **零改动**：保令牌注入与 //go:embed 列表 | 无 |
| product-design/ | 本轮已建；实施后回填「已实现/验证」状态 | 无（ci 不扫本目录） |
| docs/ | 实施阶段按 AGENTS.md:2 回填受影响的 docs（05/06/07/09/13 的界面入口描述、12 的已实现表若新增 UI 入口） | 需同步，ci 会扫 docs/ |

## 2. 先后顺序（每步可独立回退）
1. **文档基线批准**：用户确认 07 IA / 09 布局 / 18 迁移映射。
2. **index.html 结构重排**（先只搬容器，不删任何 id）：顶栏 → ContextHeader；包一层 ActivityRail + DomainView；把按钮按 18 §2 落位；确认两段精确 title+hidden 字符串逐字保留。
3. **style.css 追加层**：新外壳类 + 令牌 + 响应式扩展；跑一次 grep 确认 --danger: #f85149; 与 .card p.danger 仍在。
4. **ui2.js 追加 switchDomain**：只做显示切换 + aria-current + 二级导航展开，不重建 #sql/#results、不重绑既有 onclick。
5. **接线自检（静态）**：对 index.html / app.js / ui2.js 跑全部 18 个断言（go test ./internal/webui/...）。
6. **全量本地门禁**：go test ./internal/ci/... + pwsh -File scripts/check.ps1。
7. **文档回填**：更新 docs 里界面入口描述与 README「当前状态/内部 webui」段，保持与实现一致。
8. **真机 E2E**（见 §5，需 Playwright/内置浏览器）：主流程闭环、切域上下文、错误反馈、权限、两形态。
9. **提交推送 → GitHub Actions 发版**（用户持久偏好：本地只验证，构件走 CI；且检查在本地完成不在 release job）。本轮不提交、不推送、不发版。

## 3. 如何保持 18 条断言全绿
- 实施前把 18 个测试文件读一遍（本轮已读，见 18 提取清单），对每个 strings.Contains needle 建立「保留」清单。
- 每一步后跑 go test ./internal/webui/...；任一条红 = 立刻对照 18 定位被改动的精确字符串并还原，禁止「顺手改测试」。
- 两个最易踩：btn-new-type / btn-new-routine 的 id+title+hidden 整段、以及 irreversible_style_test.go 的 --danger/#f85149 与 .card p.danger / '<p class="danger">该变更不可逆'。
- 动态 id（btn-insert-row/metrics-panel/health-panel/btn-health-export 等）由 app.js 运行时生成，重排不碰这些函数即自动保持。

## 4. 如何不破坏 serve / desktop 两种形态
- //go:embed 列表不变、无新资源文件、无外链 CDN/字体（桌面进程内 AssetServer 无网络假设）。
- bootstrap.js 占位不变；桌面 window.DBRoverBootstrapToken 注入与团队 #t= 令牌门不变（internal/ci/desktop_test.go）。
- ui2.js detectMode 写 #mode 不变；一份文件零分叉（ADR-0017）。
- 不引入构建步骤、不切 React（按 CUT 跳过 Vercel/React 系列技能）、不新增 go.mod 依赖。
- 验收：分别起 dbrover serve（浏览器）与 dbrover-desktop（WebView2）打开同一套内嵌资源，各跑一遍主流程。

## 5. E2E 验证计划（Playwright 真实用户流，非「页面打开」）
- 用 webapp-testing / bmad-qa-generate-e2e-tests 路线写用例，覆盖：
  1. 启动 → 默认数据域 → 选连接 → 浏览对象 → 写 SELECT → 执行 → 看结果（任务完成）。
  2. 改一格 → 待提交 → 预览 → 提交 → 结果刷新 + 下游对象树可读（跨模块数据）。
  3. 切到结构/监控/AI 域再切回，连接与标签/pending 不丢（跨页上下文）。
  4. 注入错误：多语句失败块、长查询取消、NOT NULL/JSON 校验、只读写拦截，验证反馈+恢复+终态。
  5. 权限：只读连接无行级入口、能力位隐藏、R3/R4 二次确认。
  6. 两形态各跑一遍（serve 浏览器 / desktop WebView2）。
- **本轮状态**：未运行（本轮是文档设计、不改代码，无需也不应声称跑过）。阻塞见 17 报告「未验证」段。

## 6. 回退路径
每步改动都在单个内嵌文件内、按容器搬移，可 git revert 单步；id/字符串全保留意味着即便视觉重排未完成，功能仍可点。不新增 ADR（本轮不改技术栈/接口/存储/安全边界）；若实施阶段决定补备份/迁移界面并引入长任务模型（tasks 表/进度/暂停续跑），属跨模块接口+存储格式变更，须先按 AGENTS.md:2 补 ADR 与 docs/12 契约再动代码。

## 7. 进入实施前需用户拍板的决策（见回复第 5 节）
- **决策 1（方向）**：是否批准「三层 IA（ContextHeader + ActivityRail + DomainView）+ 数据域为默认主角」作为落地基线，还是倾向其他分组？
- **决策 2（范围）**：ops 域「备份/恢复/迁移界面未接入」——本轮仅保留标注空状态（现状如实），还是纳入本轮补界面入口（会引入新 UI 面与可能的新 ADR/契约）？