# 17 · UX 评审报告（quality gates + 断裂检查 + 独立复核）

> 口径：每条给 PASS / FAIL / 未验证 + 证据（file:line 或命令输出）。FAIL 列修复 + 复验。NEVER 伪造 PASS。
> 复核方法：independent-review 三输入 = (需求=用户原话+本 00 §4 目标) / (diff=本 product-design/ 全部交付物，尚未改业务代码故 diff 为空) / (测试=本轮实跑的命令，见下)。business-workflow-ux 10 点断裂检查逐条判定。

## 0. 本轮实跑的验证（诚实记录）
| 命令/动作 | 结果 | 证据 |
|---|---|---|
| 读取 internal/webui/index.html / app.js(4541 行) / ui2.js(340 行) / style.css / webui.go / bootstrap.js | 成功 | 本轮 read 工具结果 |
| 读取 18 个含 UI 断言的测试文件全文 | 成功 | type/…/irreversible_style_ui_test.go 等 |
| glob docs/** 与 adr/**、读 docs/01/05/09/11/12/13、README、AGENTS.md、check.ps1 | 成功 | 本轮工具结果 |
| 读 internal/ci/docs_test.go 与 sections_test.go 确认门禁只扫 docs/ | 成功 | docs_test.go docFiles=filepath.Join(root,"docs")、sections_test.go docsDir |
| 运行 ui-ux-pro-max scripts/search.py --design-system 与 --domain ux | 成功 | 输出 Data-Dense Dashboard + 导航/可达性规范（见 11 §0） |
| git -C F:/code/DBRover status --short / rev-parse --short HEAD | 成功 | 工作区干净、HEAD 9c07928 |
| grep 确认 internal/webui 下 backup/restore/migrate 零 UI 入口 | 成功 | 仅 compare 一处相关；docs/12 §2.6 标未实现 |
| go test ./... / go test ./internal/webui/... / scripts/check.ps1 / Playwright / 两形态真机 | **未运行** | 本轮为文档设计、未改业务代码；真机 E2E 阻塞见 §5 |
| gofmt / go vet / go build | **未运行** | 未改 Go 代码，无需；实施阶段跑 check.ps1 |

> 说明：本轮**没有修改任何业务代码**，因此「18 条断言是否仍绿」在逻辑上由 18-migration-map.md 的保留规则保证，但尚未用 go test 实测；按 quality gates 不预支 PASS，相关门禁标「未验证（实施阶段必跑）」。

## 1. 业务完整性（business-workflow-ux 10 点断裂检查）
| # | 判据 | 结论 | 证据 |
|---|---|---|---|
| 1 | 从业务入口能走完完整业务 | PASS（设计层） | 00 §6 D1、04 WF-01 六步闭环、09 数据域默认；真机未跑 → E2E 见 §5 未验证 |
| 2 | 操作有明确结果 | PASS（设计层） | 03 生命周期、14 反馈列、09 #status/result-block/notice |
| 3 | 结果被其他模块正确消费 | PASS（设计层） | 12 §2 模块依赖矩阵（对象树刷新/AI 上下文/监控读取） |
| 4 | 不需重复录入 | PASS（设计层） | 08 §4 上下文表、18 §5 不重建 #sql/#results |
| 5 | 无「功能存在但没入口」 | **FAIL** | **备份/恢复/迁移：internal/webui 全目录 grep 零 UI 入口；docs/12 §2.6 明确长任务形态未实现。这是现状缺陷，不是重设计引入。修复：D6 待拍板——(a) ops 域空状态如实标注「CLI/API 可用、界面未接入」（本方案采用，不新增假入口），或 (b) 纳入实施补界面入口（须先补 ADR+docs/12 契约）。复验：实施后 ops 域空状态可见且点了不报错；若选(b)则另有完整流程与门禁。** |
| 6 | 无无法到达的页面 | PASS（设计层） | 08 §2 合法转换、13 §3 域状态机（任意域互达，utility 抽屉可唤出可返回） |
| 7 | 无无法退出/撤销/恢复 | PASS（设计层） | 14 E18、13 finally 停表、cancelRunning/discard/dlg-cancel |
| 8 | 无管理员才懂的隐含操作 | PASS（设计层） | 02 §3、能力位显隐、环境/模式徽标 |
| 9 | 每个关键状态有合法转换 | PASS（设计层） | 13 状态机（连接/编辑器/域）无孤岛 |
| 10 | 每个异常有检测+反馈+恢复+终态 | PASS（设计层） | 14 E1–E18 表 |

## 2. 交互完整性 | 结论 | 证据
- 方向感/下一步/成功反馈/返回取消恢复/表单错误/空状态/上下文保留：PASS（设计层）—— 08/09/10/14；默认数据域 + ContextHeader 区标题 + 徽标提供方向；每弹窗有取消/关闭。

## 3. 视觉一致性 | 结论 | 证据
- 设计系统/组件/间距/表单/导航/响应式：PASS（设计层）—— 11（Data-Dense Dashboard + 追加令牌 + 组件清单 + @media 扩展）、07 §6。
- Pre-Delivery Checklist（对比度/focus/44×44/reduced-motion/375-1440）：**未验证** —— 需真机渲染后逐项；实施阶段落地。

## 4. 工程可行性 | 结论 | 证据
- 真实 API 映射/前后端状态一致/可实现权限/无重复组件/无不可能交互：PASS（设计层）—— 12 不变契约、18 迁移映射（id 全保留）、19 §1 限定 5 个内嵌文件。
- 两种形态零分叉 + //go:embed：PASS（设计层）—— 18 §8、19 §4、webui.go:1、ADR-0017。

## 5. E2E 验证 | 结论 | 证据
- Playwright 真实用户流（任务完成/无断裂/跨页数据/错误反馈/权限/两形态/业务结果）：**未验证**。
- 阻塞：本轮是文档设计、未改业务代码，交付物是设计基线；真机浏览器/Playwright 与 serve/desktop 两形态需实施落地后才能跑。用户明确要求「真机跑不起来标未验证、不要伪造 PASS」，故如实标注。
- 复验计划：19 §5 六类用例；实施后用 webapp-testing / 内置浏览器跑，结果回填本报告。

## 6. 不破坏现有功能（硬门禁）| 结论 | 证据
- 18 条 UI 断言全绿：未验证（逻辑由 18 保留规则保证，未 go test 实测；实施阶段必跑 go test ./internal/webui/...）。
- internal/ci 文档/桌面/标签门禁全绿：未验证（实施阶段必跑；且已确认 product-design/ 不被 ci 扫描，docs_test.go/sections_test.go）。
- scripts/check.ps1 全绿：未验证（实施阶段必跑；本轮未改 Go）。
- 三体验齐升 + 不破坏：PASS（设计层）—— 00 §7、10 §7、15 D1–D4；不破坏由 C1–C5 在实施阶段实测。

## 7. 独立复核（independent-review 固定结构 findings）
| # | Problem | Location | Reason | Severity | 处置 |
|---|---|---|---|---|---|
| F1 | 备份/恢复/迁移在界面无入口，却被 README/docs 列为已有能力 | 06-functional-model/fm-ops-utility.md、00 §5 | 与断裂#5 一致：能力存在、用户用不到 | should-fix | 已在设计里留 ops 域空状态并标 D6 待拍板；**不静默包装成主角**，如实 FAIL 上报 |
| F2 | 两份按钮的 title+hidden 是整段精确字符串，迁移时极易被「清理属性」误删 | 18 §2（type_ui_test.go / routine_ui_test.go） | 断言用 strings.Contains 匹配整段 | blocking（实施期） | 迁移映射逐项标注「整段保留」，实施第 2 步核对 |
| F3 | 域切换若重建 #sql/#results 会打断 ui2.js 的 renderExec 活 DOM 包装 | 18 §5、19 §4、10 §1 | ui2.js 包装 window.renderExec 并存 #results 子节点引用 | blocking（实施期） | 明文守卫「不重建节点身份、不重绑 onclick」 |
| F4 | 本轮没有任何真机/测试输出，报告不得出现预支的 PASS | 本文件 §0/§5/§6 | quality gates 禁止伪造 PASS | should-fix | 所有运行类门禁标「未验证」并写阻塞 |

复核结论：设计层业务/交互/视觉/工程门禁均 PASS；断裂#5 为**现状真实缺陷**（FAIL，已给修复与复验，且列为待拍板项）；所有真机/测试运行类门禁一律标「未验证」并写明阻塞与实施期复验计划。无「已修复并确认」的谎报；F2/F3/F4 为实施期必须回头逐条核对的项，已写入 19 计划。

## 8. 交付物清单（本 product-design/，共见回复第 3 节）
00 / 01 / 02 / 03 / 04(2) / 05(1) / 06(5) / 07 / 08 / 09(1) / 10(1) / 11 / 12 / 13 / 14 / 15 / 16 / 17 / 18 / 19。
遗漏标准目录说明：06 用 5 个功能模型文件 + 索引覆盖（按功能分组而非页面清单）；04/05/09/10 用合并文件避免碎片化（主流程四模块强耦合，见 06-functional-model/00-index.md）。

---

## 9. 实施后复验（Mode C 落地，2026-11-07 追加）

> 用户批准方案后按 `19-implementation-plan.md` 实施：只重排 `index.html` 容器、`style.css` 追加 v3 外壳层、`ui2.js` 追加 `switchDomain`，`app.js` 零改动。以下为实施后**真实跑过**的复验结果，替换上面 §0/§5/§6 的「未验证（实施阶段必跑）」预支口径。

| 门禁 | 结论 | 证据 |
|---|---|---|
| 18 条 UI 断言（`go test ./internal/webui -count=1`） | **PASS** | 重排后运行输出 `ok  github.com/maintell/DBRover/internal/webui`；两段精确 `title+hidden` 与 `--danger/#f85149` 原样保留 |
| `go test ./internal/ci -count=1` | **PASS** | 文档/桌面/标签/发版门禁全绿；`product-design/` 不被 ci 扫描 |
| `gofmt -l .` / `go vet ./...` | **PASS** | 均无输出 / exit 0 |
| `scripts/check.ps1`（含 `go test -race ./...`） | **PASS** | 全部检查通过；C: 盘 0.64GB 触发磁盘前置检查，改 `-TmpDir F:\gotmp\dbrover-check`（84GB）后全绿，非代码问题 |
| 6 条 `scripts/accept/run-local.ps1` | **PASS** | 6/6 PASS |
| `node --check ui2.js` | **PASS** | exit 0 |
| 内置浏览器真机（serve 团队模式） | **PASS** | 三层 IA 渲染、5 个域切换（数据/结构/监控标题正确、动作组按能力位显隐）、SQLite 连接加载、`SELECT 1 AS ok` 出结果 `ok/1`、console 零 error |
| Playwright 自动化全量用户流（19 §5 六类）/ desktop WebView2 真机 | **未验证** | 本轮用内置浏览器点验主流程与域切换，未编写 Playwright 自动化脚本、未起 Wails 桌面窗口；两形态中仅 serve 形态真机，desktop 形态由`//go:embed` 同一份资源 + `internal/ci/desktop_test.go` 门禁间接保证。如实标注，不伪造 PASS |

**断裂检查 #5（备份/恢复/迁移界面无入口）= 仍为 FAIL**：按用户批准的方案默认，`ops` 域仅显示如实空状态「命令行与 API 可用，界面入口未接入」，不新增假入口、不伪装主角；补界面入口需另拍板（可能补 ADR + `docs/12` 契约）。独立复核 F2（精确字符串保留）/F3（不重建 `#sql`/`#results` 节点身份）已由「app.js 零改动 + ui2.js 仅切动作组 `[hidden]`」落地并实测通过。
