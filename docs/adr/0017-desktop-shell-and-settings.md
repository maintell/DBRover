# ADR-0017 · 桌面壳、进程模型与服务端设置

- 状态：**已采纳**
- 日期：2026-11-06
- 影响面：桌面形态、进程模型、跨模块接口、安全边界、存储格式、新依赖、CGO 边界
- 关联：[ADR-0001](0001-tech-stack.md)、设计说明 [../superpowers/specs/2026-11-06-desktop-shell-and-ux-design.md](../superpowers/specs/2026-11-06-desktop-shell-and-ux-design.md)

---

## 背景

用户明确要求把「仅 serve + 浏览器」的工具改造成可独立分发的应用，同时保留团队模式，并补齐交互短板（设置缺失、连接管理、编辑器、视觉）。
此前 ADR-0001 §7 只给了「Wails v2（建议）」，状态是「待确认」；`cmd/dbrover-desktop` 一直是空占位。

约束（来自 AGENTS.md 与 docs/02）：核心免 CGO、可交叉编译；`api → service → driver` 分层不破；凭据永不落明文；本地服务只监听回环、token 每次启动重生成；发版只由 tag 触发且只有一条流水线。
本机磁盘紧张，不为出产物做本地大型编译，产物走 CI。

---

## 决策

### 1. 两种形态并存，共用一份内核

`dbrover-desktop`（原生窗口）与 `dbrover serve`（浏览器）都构造同一个 `api.Server`，加载同一份 `internal/webui` 静态资源，经同一套执行门、审计、能力边界触达数据。
桌面壳不是第二套实现，只是「同一进程里换一种承载方式」：serve 把 Handler 挂到 loopback 监听，桌面把 Handler 挂到 WebView2 的进程内资源处理器。

**否决**：把桌面做成单独一套 IPC/RPC 协议；那样两份实现会各自演化，团队模式与桌面迟早不一致。

### 2. 桌面进程模型：无监听端口

Wails v2 的 `assetserver.Options` 允许 `Assets: nil, Handler: h`：官方源码注释明确「If set to nil, all GET requests will be forwarded to Handler」，且 `Options.Validate()` 只要求三者之一非空。
因此桌面窗口的所有请求（`/'、/app.js'、/style.css'、/api/v1/*`）都走 `api.Server.Handler()`，由 WebView2 在进程内直接派发，**不打开任何 TCP 端口**。

这比 serve 更安全：没有端口就没有本机其他进程探测、没有跨站请求面；token 仍保留，因为 `withAuth` 是共用 Handler 的一部分，不能为桌面开特例。

**否决**：桌面另起一个 loopback 服务再让 WebView 去连。多一个端口、多一份 token 打印与生命周期管理，且没有收益。

### 3. 跨模块接口：`internal/host`

新增 `internal/host` 作为「一条进程所需依赖」的装配层，供两个入口复用：

- `host.OpenConnector`：按引擎分发连接器（从 `cmd/dbrover` 抽出，CLI 子命令也改调它，避免两份工厂）。
- `host.New(Options) → *Runtime`：装配审计日志、审计锚点密钥、指标留存、设置存储、`api.Server`、Agent 配置。
- `host.DesktopHandler(s, token) → http.Handler`：纯 `net/http` 包装，注入 bootstrap 脚本、对齐 WebView 伪造请求的 Host，其余委托 `s.Handler()`。**不含 Wails 依赖**，因此能在根 module 里被单测覆盖。

依赖方向：`cmd/* → host → {api, service, driver/*, infra/secret, infra/settings}`，`api → {service, driver, infra/settings, webui}`。`host` 不反向被 `api` 引用，`driver` 不引用 `service`，分层铁律不破。

`cmd/dbrover` 的 `serve` 子命令保留全部开关与行为，只把装配过程委托给 `host.New`；CLI 的 `health/tables/exec` 等仍走 `host.OpenConnector`。

### 4. 令牌注入

- `api.New("")` 每次生成随机 token，只在进程内存。
- 桌面壳把该 token 交给 `host.DesktopHandler`，由它拦截 `GET /bootstrap.js` 返回 `window.DBRoverBootstrapToken="<token>";`（内存中拼装，`no-store`）。
- `index.html` 在 `app.js` 之前引入 `/bootstrap.js`；serve 模式由 `api.Handler()` 提供同名空脚本，令牌仍来自 URL 片段 `#t=`。
- `app.js` 取令牌优先级：URL 片段 > 注入的全局量 > 会话存储。三种都只是「让同一个 `state.token` 有值」，后续请求头逻辑不变。
- 令牌不打印到 stderr、不写文件、不进审计与日志。桌面没有可复制的链接，这正是它比 `serve -open` 更内聚的地方。

### 5. 安全边界

- 桌面无监听端口；`withAuth` 的 token 与 Origin 校验照常生效。
- WebView2 的请求由 Wails 伪造，`Origin` 与 `r.Host` 可能不一致；`host.DesktopHandler` 在**进程内**把 `r.Host` 对齐到 Origin 的 host，使 `originAllowed` 的同源判定成立。这不是放宽校验：桌面没有网络入口，所有请求都来自本窗口，且改动只在桌面包装层，不碰 `api.originAllowed`。
- 连接默认只读、生产库写需审批、查询历史不记结果集：全部沿用，不由设置或桌面形态放宽。
- 设置里的 API Key 只进 OS 凭据库；`secret.*` 在非 Windows 平台返回 `ErrUnsupported`，不退化成明文文件。

### 6. 存储格式：服务端设置

- `GET/PATCH /api/v1/settings`。`/api/v1/settings` 进入 `routes()` 与 `openapi.json`，受路由双向门禁与请求体门禁约束。
- 非敏感项：单文件 JSON（0600），字段带稳定名称，未知字段按现有 `decode` 约定拒绝。两种模式指向同一份配置文件，故设置跨形态一致。
- 敏感项：`api_key` 经 `secret.Store(db/agent-settings, api-key, value)` 写入 OS 凭据库；`GET /settings` 只回 `api_key_configured: bool`，绝不回值。
- 优先级：设置 > 环境变量 > 凭据库（环境变量保留 CI 覆盖能力）。

**否决**：浏览器 localStorage。桌面与团队模式会各存一份，刷新/换端就丢，且与「设置是服务端能力」的契约不符。

### 7. 新依赖与 CGO 边界

- 新增 `github.com/wailsapp/wails/v2 v2.10.2`（已核对 go.mod 声明 `go 1.22.0`，与根 `go 1.26.0` 兼容；Windows 侧走 `go-webview2`，纯 Go syscall，无 CGO）。
- 该依赖**只在 `cmd/dbrover-desktop` 的独立 module 里**：根 `go.mod` 不增加任何依赖，`CGO_ENABLED=0 go build ./...` 与五平台 CLI 交叉编译不受影响。
- 嵌套 module 自带 `go.mod`（`go 1.26.0`）与 `replace github.com/maintell/DBRover => ../..`；Go 会把它排除在根 `./...` 之外。
- Wails 的 macOS / Linux 构建需要 CGO（gtk/webkit），与本仓库免 CGO 矩阵冲突：**本轮只发 Windows 桌面包**，其余平台桌面壳需另开 ADR 与发版线。

### 8. 界面

复用免构建 `internal/webui`，追加式改动：保留所有已验收的 id、动作名、能力位引用与源码断言，新增设置面板、连接面板、多标签编辑与视觉系统。React + TS + Vite 仍是 ADR-0001 §6 的长期目标，本轮不启动。

### 9. 发版

- 不新建 workflow。Windows 桌面 job 并入现有 `release.yml`，与五平台 CLI 构建同属 `push.tags: ['v*']`、固定并发组 `release`、单条 `publish`。
- release.yml 仍不含 `go test` / `gofmt` / `go vet`；质量检查在打 tag 前由 `scripts/check.ps1` 本地完成。
- PR 只跑测试（ci.yml），不启动打包。

---

## 后果

**正面**

- 一份内核两种承载，设置与行为天然一致；桌面连端口都不开，攻击面更小。
- 根 module 零新增依赖，既有 CI 与交叉编译矩阵不动。
- 设置成为服务端能力，桌面与团队模式、重启前后都一致。

**代价 / 需承担**

- 桌面只发 Windows；macOS / Linux 用户仍用 `dbrover serve -open`。
- 嵌套 module 需要单独 `go mod tidy`，CI 多一条 Windows job。
- 多标签编辑器保留单 `#sql` 文本节点，切换标签靠换值，标签内容只在前端内存里，刷新不恢复（未做工作区持久化，属已知限制）。
- `/bootstrap.js` 是新增的静态资源，需在 `api.Handler()` 里为 serve 模式提供空实现，避免 404 噪声。

---

## 验收

- `internal/host` 单测覆盖：bootstrap 注入的令牌与 `api.Server.Token()` 一致、空 bootstrap 时退回片段令牌、资源处理器不监听端口（桌面包装层只做委派）、Host 对齐。
- `internal/infra/settings` 单测覆盖：往返、未知字段拒绝、API Key 不进 JSON、非 Windows 凭据库降级明确报错。
- 端点进 `openapi.json` 与 docs/12 已实现表，`TestSpecMatchesRoutes` 与请求体门禁全绿（含变异验证）。
- 既有全部测试、6 条本机端到端脚本、`scripts/check.ps1` 全绿。
- release.yml 仍通过 `internal/ci` 的全部断言。

---

## 相关

- [ADR-0001 技术栈与进程模型](0001-tech-stack.md)
- [ADR-0005 跨引擎差异归能力与方言](0005-engine-differences-as-capabilities.md)
- [ADR-0016 指标留存用单文件 SQLite](0016-metric-retention-store.md)
- [docs/02 架构](../02-architecture.md)
- [docs/10 连接与安全](../10-connection-and-security.md)
- [docs/12 API 契约](../12-api-contract.md)
