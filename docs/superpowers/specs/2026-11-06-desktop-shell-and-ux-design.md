# DBRover 桌面壳与交互重做 · 设计说明

- 日期：2026-11-06
- 状态：已批准，进入实现
- 关联决策：[ADR-0017](../../adr/0017-desktop-shell-and-settings.md)、[ADR-0001](../../adr/0001-tech-stack.md)

## 目标

把 DBRover 从「仅 serve + 浏览器」扩展为两种并存的形态，并补齐交互短板：

1. **桌面形态**：一个可独立分发、双击即用的原生窗口应用（Windows 优先）。
2. **团队形态**：保留现有 `dbrover serve` + 浏览器，语义与桌面完全一致。
3. **交互四项**：设置面板、连接管理流程、编辑器与结果区、整体视觉与布局重做。
4. **零分叉**：两种模式共用同一份 `api.Server`、同一份 `internal/webui`、同一套执行门与审计。
5. **正式前端仍是长期目标**：本轮复用免构建的 `internal/webui`，不切换到 React + Monaco。

## 形态对照

| | 桌面 | 团队 |
|---|---|---|
| 入口 | `dbrover-desktop` | `dbrover serve` |
| 承载 | Wails v2 + WebView2 原生窗口 | 系统浏览器 |
| 服务承载 | 进程内资源处理器，**不监听端口** | loopback HTTP，随机 token |
| 界面 | 内嵌 `internal/webui` | 同一份 `internal/webui` |
| 设置 | 服务端持久化，同一份配置文件 | 同左 |
| 打包 | Windows 单 exe，CI 产出 | 五平台 CLI，CI 产出 |

## 桌面进程模型

- 构造 `api.Server`（随机 token），把 `s.Handler()` 交给 Wails 的 `AssetServer.Handler`。
- Wails v2 在 Windows 上通过 WebView2 自定义资源处理器把请求交给该 Handler，**不经过 TCP**，因此没有端口、没有本机越权面。
- 令牌通过进程内返回的 `/bootstrap.js` 注入窗口：内容只有 `window.DBRoverBootstrapToken="<随机 token>"`，只在内存里生成，不打印、不落盘、不进日志。
- 窗口加载 `/` 得到 `index.html`；`app.js` 依次从 URL 片段、注入的全局量、会话存储里取令牌，后续每个 API 请求带 `X-DBRover-Token`。
- 关窗 → `wails.Run` 返回 → 关闭连接、指标留存、审计、设置存储 → 进程退出。
- WebView2 缺失 / 窗口创建失败 / 资源处理器初始化失败：明确报错并提示可用 `dbrover serve -open` 回退，不静默白屏。

## 构建隔离

- Wails 代码放在 `cmd/dbrover-desktop` 的**独立 Go module**（自带 `go.mod` + `replace` 指向仓库根）。
- 根 module 的 `go build ./...` / `go test ./...` / `go vet ./...` 天然跳过嵌套 module，纯 Go 交叉编译与测试不被 Wails 拖住。
- 桌面壳的编译验证放在 CI；本机磁盘紧张，不做本地 Wails 产物构建。
- Wails v2 在 Windows 上不需要 CGO；macOS / Linux 的 Wails 需要 CGO，与本仓库「免 CGO 交叉编译」冲突，本轮**只发 Windows 桌面包**，其余平台桌面壳留给后续 ADR。

## 设置

- 服务端 `GET /api/v1/settings` 与 `PATCH /api/v1/settings`。
- 非敏感项落单文件 JSON（0600），两种模式共用同一份配置文件，因此「桌面里改的主题/字号/模型配置，在团队模式里打开仍是同一份」。
- 模型服务的 API Key **不进 JSON**：写入 OS 凭据库，读取接口只回「已配置 / 未配置」，永不回显值。
- 环境变量仍作为覆盖项（CI 与临时调试用），优先级：设置 > 环境变量 > 凭据库。
- 设置变更会影响下一次 Agent 会话的模型配置；不改安全边界（默认只读、写操作审批都不由设置放宽）。

## 交互四项

- **设置面板**：主题（深/浅）、界面字号、编辑器选项、模型服务（base_url / model / context_tokens / API Key）、快捷键说明。
- **连接管理**：独立的连接面板，列出/切换/测试/删除连接，空状态有引导；「编辑」明确要求重新输入口令（口令只进不出，服务端不回显），不伪装成无副作用的原地编辑。
- **编辑器与结果区**：多标签 SQL 工作区，每个标签独立的语句与结果；保留单一 `#sql` 文本节点作为当前活动编辑器，切换标签时换内容，避免重做已验收的执行/审批链路。
- **整体视觉与布局**：新的 CSS 设计令牌（深/浅两套）、分组工具条、清晰的状态与空状态、一致的间距与层级；多余标语与说明文字删掉，靠布局而非文案让人看懂。

## 实施顺序

1. 文档：本说明、ADR-0017、ADR-0001 状态、docs 回填。
2. 共享内核 `internal/host`：连接器工厂、审计/锚点/指标/设置装配、桌面资源处理器，`cmd/dbrover serve` 改为调用它。
3. 服务端设置：类型、存储、端点、契约、门禁。
4. 界面：结构、视觉、交互模块（追加式，保留既有已验收行为与源码断言）。
5. 桌面壳：Wails 入口。
6. CI：release.yml 并入 Windows 桌面 job。
7. 验证：静态、单测、端到端脚本、桌面模块编译、门禁变异验证。
8. 提交推送，产物交给 CI。

## 验收口径

- 两种模式共用同一份 `api.Server` 与 `internal/webui`：由源码门禁保证（界面路径、能力位、动作枚举、OpenAPI 路由双向一致）。
- 桌面壳不监听端口：`AssetServer.Handler` 不带 `Assets`，所有请求进程内处理，有单测覆盖令牌注入与「无 bootstrap 时退回片段令牌」。
- 新增端点进 `openapi.json` 与 docs/12 的已实现表，路由双向门禁保持绿。
- 设置持久化跨两种模式一致，API Key 不出现在任何响应、日志与配置文件里。
- 既有全部测试与 6 条本机端到端脚本保持全绿。
- 发版仍只有一条流水线、一个并发组、一个 publish，PR 只跑测试。
