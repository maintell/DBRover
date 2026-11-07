# 15 · 验收标准（可验证，PASS/FAIL/未验证 在 17 报告里判）

## A. 业务完整性（business-workflow-ux 断裂检查，逐条）
| # | 判据 | 验收方式 | 证据 |
|---|---|---|---|
| A1 | 打开应用能从「数据」入口走完完整主流程（选连接→浏览→写跑 SQL→看结果→改数据→提交） | Playwright 真实用户流（webapp-testing） | WF-01 |
| A2 | 每步有明确结果（成功/失败/部分成功 + 下一步） | result-block / #status / notice / 预案预览 | WF-01、14 |
| A3 | 结果被其他模块正确消费（对象树/结构刷新、AI 上下文、监控读取） | 跨模块检查 | 12 §2 |
| A4 | 跨页不重复录入（连接、选中对象、标签、pending 保留） | 切域后断言 state.current 不变 | 08 §4 |
| A5 | 无「功能存在但没入口」 | 全功能入口盘点 | **FAIL：备份/恢复/迁移界面未接入（D6/E17）** |
| A6 | 无无法到达的页面 | 导航合法转换 | 08 §2、13 §3 |
| A7 | 无无法退出/撤销/恢复的流程 | 弹窗取消、pending 丢弃、cancelRunning、一键撤销 | 14 E18 |
| A8 | 无管理员才懂的隐含操作 | 能力位显隐 + 环境/模式徽标 | 02 §3 |
| A9 | 每个关键状态有合法转换 | 状态机 | 13 |
| A10 | 每个异常有检测点+反馈+恢复+终态 | E1–E18 | 14 |

## B. 聚焦主要功能的重排（可量化）
- B1 默认激活域 = data，启动到可写第一条 SQL 零导航点击。
- B2 一级入口 ≤5；顶栏按钮数从 ~15 降到 ≤4 且不随新功能增长。
- B3 新建序列/类型/例程、体检/会话/指标、比对 不再常驻顶栏，进对应域二级。
- B4 #btn-run 为唯一最高视觉权重主操作；编辑器/结果区占最大面积。
- B5 文案克制：删掉顶栏长 title 标语后，靠 ContextHeader 徽标+布局仍能看懂，未加回说明文字。

## C. 不破坏现有功能（硬门禁）
- C1 go test ./internal/webui/... 全绿（18 个 UI 断言文件）。
- C2 go test ./internal/ci/... 全绿（docs/desktop/tab_results/policy/readme_flags/sections）。
- C3 pwsh -File scripts/check.ps1 全绿（gofmt/vet/build/test；-race 本机无 gcc 时按脚本 skip，CI 覆盖）。
- C4 两种形态：serve 浏览器与 desktop WebView2 加载同一份内嵌文件、令牌链路不变、零构建。
- C5 所有 /api/v1/* 路由、action 字面量、capabilities 键名、导出格式、危险色不变。

## D. 三体验齐升
- D1 用户：主流程少点击、≤5 一级入口（B1/B2）。
- D2 开发：新能力进对应域组、不改顶栏；结构清晰（07/19）。
- D3 AI Agent：data-domain/data-action/aria-label + 稳定 id、报错明确、可脚本化（08 §6/18）。
- D4 原有功能未破坏（C1–C5）。

## E. 视觉一致性（ui-ux-pro-max）
对比度 4.5:1、focus 可见、≥44×44、键盘可达、reduced-motion、375/768/1024/1440 响应式、Pre-Delivery Checklist（11 §7）。