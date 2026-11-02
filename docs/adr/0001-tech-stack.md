# ADR-0001 · 技术栈与进程模型

- 状态：**建议（待确认）**
- 日期：2026-11-02
- 影响面：全项目

---

## 背景

需要同时满足四个硬约束（见 [02-architecture §1](../02-architecture.md#1-设计约束先于方案)）：

- C1 UI 与 Agent 能力一致
- C2 桌面单机可用、团队可服务化
- C3 免 CGO、可交叉编译单文件
- C4 所有写操作经统一门

以及现实约束：跨平台分发（Windows 为主）、团队 Go 经验、发布走 CI。

---

## 决策

### 1. 核心：Go 1.23+ 单一二进制，免 CGO

**理由**

| 维度 | Go | 说明 |
|---|---|---|
| 交叉编译 | 强 | `GOOS/GOARCH` 一行命令，CGO 关闭后无工具链依赖 |
| 驱动生态 | 满足 | 8 种引擎均有纯 Go 驱动（含 `modernc.org/sqlite`、`sijms/go-ora`） |
| 并发模型 | 贴合 | goroutine 天然映射「多连接、多流、多取消」 |
| 单文件分发 | 满足 | 无需运行时，安装包体积可控 |
| 团队技能 | 匹配 | — |

**放弃的备选**

| 备选 | 否决原因 |
|---|---|
| Rust | 驱动生态分散（Oracle/MSSQL 需自行封装），编译慢，迭代成本高 |
| JVM（DataGrip 路线） | 需打包 JRE，安装包 200MB+，冷启动慢 |
| .NET（dbForge 路线） | Linux/macOS 体验与打包不如 Go 直接；跨平台 GUI 生态弱 |
| Node/Electron 主进程直连数据库 | 驱动质量参差（尤其 Oracle），且无法复用同一套服务化 API |

### 2. 进程模型：核心是本地服务，UI 是纯 SPA

**决策**：核心以 loopback HTTP + WS 暴露能力；桌面壳只是一层窗口。

**理由**

1. 服务化零成本（改监听地址 + 加认证）。
2. 前端可独立开发（Mock server）。
3. 可 `curl` 排障。
4. 崩溃隔离：UI 崩了核心仍在跑长任务。

**代价**：一层序列化开销与协议维护。可接受——结果集用 WS 二进制分片，非 JSON。

### 3. HTTP：标准库 `net/http` + `coder/websocket`

不引 gin/echo：路由需求简单（Go 1.22 的 `ServeMux` 支持方法与路径参数），依赖越少越容易长期维护。

### 4. 契约：OpenAPI 3.1 手写为真相 + 代码生成

不用「代码优先再生成 spec」——那样契约永远是实现的影子，无法约束实现。

### 5. 元数据存储：SQLite（`modernc.org/sqlite`，纯 Go）

单文件、可备份、SQL 查询方便、支持 FTS5（用于对象搜索）。

### 6. 前端：React 19 + TS strict + Vite + Monaco + TanStack

**Monaco 而非 CodeMirror**：SQL 高亮的方言覆盖、补全 API、性能（大文件）都更成熟；且用户对 VS Code 的交互有肌肉记忆。

### 7. 桌面壳：Wails v2（建议）

| 维度 | Wails v2 | Tauri 2 |
|---|---|---|
| 语言数 | Go + TS（**2**） | Rust + Go + TS（**3**） |
| 与核心集成 | 同进程，直接嵌入 | 需 sidecar 管理生命周期 |
| Windows 打包 | 简单（WebView2 内置或引导安装） | 简单 |
| macOS/Linux | 需 CGO（gtk/webkit） | 无需 CGO |
| 更新器 | 需自建 | 内置 |

**选 Wails**：语言数少一半、与 Go 核心同进程、团队无需 Rust。CGO 只影响非 Windows 平台，交给 CI 矩阵。

**关键前提**：因为核心是 loopback 服务、UI 是 SPA，**壳可替换**。若 Wails 在 macOS/Linux 打包失败，换 Tauri 或「托盘 + 系统浏览器」只影响壳层。因此这个决策的风险低。

---

## 后果

**正面**

- 一份代码三种形态（桌面/服务/无头），语义一致。
- 无运行时依赖，安装包小。
- 新增引擎成本可预测（见 04 文档 §8 检查清单）。

**负面 / 需要承担**

- 非 Windows 平台若选 Wails 需 CGO 构建 → CI 矩阵复杂化，release job 不可纯 `GOOS/GOARCH` 交叉编译。
- `modernc.org/sqlite` 性能低于 CGO 版（元数据库数据量小，可接受；但若将来做「结果集持久化」需重新评估）。
- Oracle 通过 `go-ora` 的能力覆盖需 POC 验证；不足时需接受 CGO（届时修订本 ADR）。
- 前端与核心虽同仓，仍需维护契约稳定性。

---

## 待验证

| 项 | 方法 | 何时 |
|---|---|---|
| `go-ora` 能否覆盖 Oracle 主要场景 | 连真实实例，跑 `drivertest` 基础套件 | M1 内 POC |
| Wails 在 macOS/Linux 的打包可行性 | CI 试构建一次 | M1 内 |
| `modernc.org/sqlite` 在万级元数据对象下的查询性能 | 基准测试 | M1 |

---

## 相关

- [02-architecture](../02-architecture.md)
- [ADR-0002](0002-agent-tool-boundary.md)
- [ADR-0003](0003-plan-apply-separation.md)
