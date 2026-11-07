# 10 · 交互与状态规格

## 1. 域切换 switchDomain(target)
- 触发：点 ActivityRail 入口 / 键盘（Tab+Enter，focus 可见）。
- 行为：切换 aria-current 与 DomainView 显示；**不改** state.current、不清 ui2.tabs、不重建 #sql/#results 节点、不重绑 onclick（防 ui2.js 包装失效）。
- 反馈：ContextHeader 区标题更新；无整页刷新；窄屏抽屉自动收起。
- 异常：目标域所需能力位缺失 → 二级项 hidden，一级入口仍可达并给出「当前引擎不支持」说明（E4）。

## 2. 主要动作强化（聚焦）
- #btn-run：.primary 主色、≥44×44、Ctrl+Enter 全局可用；其视觉权重高于所有其他按钮。
- 编辑器/结果区占视口最大面积（Data-Dense Dashboard：最小 padding、最大数据可见），对象树可折叠但默认展开。
- 次要动作（计划/执行全部/取消/遇错继续/清空结果）降级为次级按钮或图标+aria-label，不删除、不改名。

## 3. 渐进披露
- 二级动作默认折叠/收纳在所属域面板；「新建序列/类型/例程」「体检/会话/指标」不再常驻。
- 能力位 hidden=!caps.xxx 决定子项显隐（type_ui_test.go / metrics_ui_test.go 钉死）。

## 4. 表单/反馈（ui-ux-pro-max §8）
- 错误就近显示：NOT NULL/NULL 三态、JSON 不合法「内容还在」、长文本全文编辑器不截断写回。
- 不可逆用 .danger 红（--danger:#f85149 被 irreversible_style_test.go 钉死，不得改）。
- 空状态有引导（连接空、结果空、ops 未接入、无锁等待、无指标留存）。
- 成功反馈：#status、showNotice、结果块；写操作附回滚/撤销入口。

## 5. 键盘/可达性（ui-ux-pro-max §1/§9）
- Tab 顺序与视觉顺序一致；focus 环可见；图标按钮 aria-label；prefers-reduced-motion 降级；hover 不是唯一发现途径。
- 抽屉/弹窗有取消/关闭与 Esc 退出通道；无键盘陷阱。

## 6. 文案克制（用户持久偏好）
- 删除顶栏长 title 标语、次要标签；一级导航用单词 + 图标。
- 若删完看不懂「当前在哪个域/哪条连接」：靠 ContextHeader 的区标题 + #env/#identity/#profile 徽标 + 布局层级解决，**不把说明文字加回来**。

## 7. 三体验齐升的可验证点
- 用户：主流程零导航点击、≤5 一级入口、少点击（07 §4）。
- 开发：新能力进对应域组、不改顶栏；容器职责清晰（19）。
- AI Agent：data-domain/data-action/aria-label + 稳定 id、明确错误文案、可脚本化（18/19）。
- 不破坏：18 断言 + 两形态 + check.ps1 全绿（17）。