# 11 · 设计系统（令牌 / 间距 / 字号 / 配色 / 组件清单）

> 依据 ui-ux-pro-max 实跑 search.py --design-system 命中 Data-Dense Dashboard（浅/深双模式、最小 padding、网格、最大数据可见）与 quick-reference.md §1/§2/§6/§8/§9。检索同时返回 Enterprise Gateway 营销站 pattern，与工作台不适配，按 SKILL 规则声明为 fallback，不采用。

## 1. 原则
- **不替换现有被钉死的令牌**：--danger: #f85149; 与 .card p.danger { color: var(--danger); } 被 irreversible_style_test.go 断言，必须原样保留。
- **追加式**：现有 style.css 已是「原始层 + v2 追加层」双段（原始层与 v2 · 视觉与布局层 注释段）；重排沿用「追加层」模式，不改已验收规则。
- 无构建、无外链字体（ui-ux-pro-max 建议的 Fira Code/Sans 仅作参考，//go:embed 不引外部资源）；继续用系统字体栈 var(--mono)、system-ui。

## 2. 现有令牌（保留）
--bg --panel --panel-2 --line --fg --muted --accent --ok --warn --danger --mono --ui-font-size --tab-size --radius --shadow --header-h，深/浅两套（body[data-theme=light]）。

## 3. 追加令牌（建议，实施阶段落地）
| 令牌 | 值（建议） | 用途 |
|---|---|---|
| --rail-w | 56px | ActivityRail 宽（图标带） |
| --space-1/2/3/4 | 4/8/12/16px | 间距刻度（dashboard 高密度） |
| --fs-11/12/13/15 | 11/12/13/15px | 字号刻度（quick-ref §6：body ≥12、base 13/16） |
| --z-rail | 5 | rail 层级（低于 drawer 40 / overlay 50） |
| --radius | 保留 8px | 组件圆角 |
| --focus | var(--accent) | focus 环（2px，quick-ref §1） |

## 4. 配色语义（不改值，只定用法）
- --accent = 主操作（#btn-run .primary）/ 激活域 aria-current。
- --danger = 不可逆/终止会话（红，被钉死）；--warn = 需注意（approval 左边框、R3 徽标）；--ok = 成功/只读安全。
- 环境徽标沿用现有 .env-production 等（FR-ENV-12 持续可见）；#risk/#friction 沿用 .r0–.r4 分级色。
- 对比度：正文 ≥4.5:1、大字 ≥3:1（quick-ref §1）；不靠颜色单独传达（风险徽标带文字）。

## 5. 组件清单（沿用 + 新增外壳）
**沿用（不改 id/类名）**：button/.primary/.danger、.badge/.r0–.r4/.env-*、.tool-group/.toolbar、.tabbar/.sql-tab/.tab-x/.tab-add、aside/.side-head/.object-tree/.obj-search、#sql/.editor-wrap、table/th/td(.editable/.dirty/.null/.generated)、.result-block/.block-head、.notice(.error/.warn)、.overlay/.card/.drawer/.drawer-panel、.conn-card/.set-group/.set-row/.keys/kbd、.chip/.cell-menu/.schema/.approval、.empty-state。

**新增（纯外壳，不改业务）**：.context-header、.activity-rail、.rail-item[aria-current]、.domain-view、.domain-title、.secondary-nav、.ops-empty。

## 6. 布局/密度
- 桌面优先、Data-Dense：--space-2=8px 为默认间隙，结果网格 white-space:nowrap + 横向滚动（现状），不塞大 padding。
- @media (max-width:900px) 已存在（隐藏 #schema-tools、aside 收窄到 200px），扩展为收起 ActivityRail，不重写。
- 动画：150–300ms hover 过渡；prefers-reduced-motion 降级（quick-ref §7）。

## 7. Pre-Delivery Checklist（落地时逐项，不在这里伪造通过）
无 emoji 作图标 · 可点元素 cursor:pointer · hover 过渡 · 浅色对比度 · focus 可见 · reduced-motion · 375/768/1024/1440 响应式。当前为设计基线，标「未验证」，真机/Playwright 门禁见 17。