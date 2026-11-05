# 02 · 总体架构

> 目标：把「8 种引擎的差异」和「人与 Agent 的一致授权」两件事同时装进一个可维护的结构里。

---

## 1. 设计约束（先于方案）

四个约束决定了架构形状，任何方案都要先满足它们：

| # | 约束 | 派生要求 |
|---|---|---|
| C1 | UI 与 Agent 必须拥有**完全相同的能力集** | 能力必须收敛为服务层 API，不存在「只有界面能做」或「只有 AI 能做」的操作 |
| C2 | 桌面单机可用，团队可服务化 | 核心与界面解耦，核心可独立进程运行 |
| C3 | 免 CGO，能交叉编译出单文件 | 驱动、SSH、SQLite 全部选纯 Go 实现 |
| C4 | 任何写操作都要能审计与回滚 | 执行必须经过统一的「执行门」，不能有旁路 |

---

## 2. 分层与依赖方向

```
┌─────────────────────────────────────────────────────────────┐
│  Presentation                                                │
│  web/ (React SPA)          │  desktop shell (WebView)        │
└────────────────────────────┼────────────────────────────────┘
                             │  HTTP(JSON) + WebSocket
┌────────────────────────────▼────────────────────────────────┐
│  api/          协议转换、鉴权 token、错误映射、事件推送        │
├─────────────────────────────────────────────────────────────┤
│  agent/        Agent 运行时：工具注册、规划、审批、审计        │
├─────────────────────────────────────────────────────────────┤
│  service/      业务编排（唯一权限判断点）                     │
│    connection  query  metadata  change  migration            │
│    monitor     backup  audit     workspace                   │
├─────────────────────────────────────────────────────────────┤
│  driver/       Connector 接口 + 引擎实现 + dialect/          │
├─────────────────────────────────────────────────────────────┤
│  infra/        ssh tunnel · credential store · meta store    │
│                logger · metrics · fs                         │
└─────────────────────────────────────────────────────────────┘
```

**依赖铁律**

1. 依赖只能向下。`driver` 不认识 `service`，`service` 不认识 `api`。
2. `agent` 与 `api` **并列**，都只调用 `service`。这样 Agent 天然继承全部权限与审计。
3. `driver` 的返回值是**中立模型**（`model.*`），不得泄漏引擎私有类型到上层。
4. 唯一允许的跨层例外：`model` 被所有层引用。

---

## 3. 进程模型

同一份核心，三种运行形态：

| 形态 | 入口 | 监听 | 用途 |
|---|---|---|---|
| **桌面（正式）** | `dbrover-desktop`（Wails 壳 + 内嵌核心） | 内核只监听 `127.0.0.1:<随机>`，壳通过进程内 loopback 访问 | 个人日常 |
| **服务** | `dbrover serve --addr 127.0.0.1:8080` | 可配置地址；默认仍只绑回环 | 团队/容器/远程开发机 |
| **桌面（轻量，已可用）** | `dbrover serve -open` | 同上；就绪后自动用默认浏览器打开带 token 的地址 | 没有桌面壳时也能「双击就用」 |
| **无头** | `dbrover exec -db <文件> (-sql "…" \| -file <路径>)` | 不监听 | CI 校验、脚本、Agent 外部调用 |

三者共用 `service` 层，因此「CI 里跑的 SQL 检查」与「界面上点的执行」语义一致。

### 3.1 为什么不是「纯库 + 直接函数调用」

桌面形态完全可以在一个进程里直接调函数，省掉 HTTP。之所以统一走 loopback HTTP，是因为：

- 服务化零成本（改个监听地址）；
- 前端可脱离 Go 独立开发（Mock server）；
- 崩溃隔离：UI 崩了核心还能给 Agent 跑长任务；
- 可以对着 `curl` 排障，降低「只能靠点界面」的调试成本。

代价是序列化开销与一层协议。目标是结果集用 WS 流式 + 紧凑二进制分片；**v1 实际是 HTTP JSON**（`?stream=1` 走 NDJSON），WS 与二进制分片都未实现。

### 3.2 端口与安全

- 启动时生成随机 token（32 字节），**只在进程内存里**，并打印到 stderr（含 `#t=` 的界面链接）；不落盘、不写临时文件。
- 所有请求校验 `X-DBRover-Token`。
- 绑定非回环地址需要显式 `-allow-remote`，否则拒绝启动。

> **v1 实现口径（安全相关，必须说清）**：**TLS 未实现**。`-allow-remote` 打开之后服务就是明文 HTTP，
> 只应放在受信网络或反向代理之后——这一条是目标，不是现状。
>
> **`Origin` 校验已实现**（此前这里写着「还没有实现」，是过时说法）：`api.originAllowed`
> 只放行「不带 `Origin`」（curl / 桌面壳）与「同源」，其余返回 403 `ORIGIN_NOT_ALLOWED`；
> 刻意不接受 `null`（它来自 `file://` 或沙箱 iframe）。用例：`internal/api/origin_test.go`。

---

## 4. 技术选型

### 4.1 后端

| 关注点 | 选择 | 理由 | 备选与否决原因 |
|---|---|---|---|
| 语言 | **Go 1.23+** | 免 CGO 交叉编译、并发模型贴合「多连接多流」、单文件分发 | Rust：驱动生态分散、编译慢；JVM/.NET：需要运行时 |
| HTTP | 标准库 `net/http`（1.22+ 路由）+ 少量中间件 | 依赖最少 | gin/echo：功能冗余 |
| WebSocket | **未引入**（v1 用 NDJSON 流；目标是 `coder/websocket`） | context 感知、API 干净 | gorilla：维护模式 |
| 契约 | 手写 OpenAPI 3.1 为唯一真相 + `oapi-codegen` 生成 Go server 类型与 TS client | 契约先于实现，前端不手写 fetch | 代码优先生成 spec：易漂移 |
| 元数据存储 | **SQLite / `modernc.org/sqlite`**（纯 Go） | 单文件、可备份、免 CGO | 自重实现的 KV：查询与迁移麻烦 |
| 迁移 | 内嵌 SQL 迁移（`embed` + 版本表） | 不引重量级框架 | gorm/ent：不需要 ORM |
| 日志 | `log/slog` + 结构化 JSON | 标准库，字段化 | zap：可用但无必要 |
| 指标 | 进程内环形缓冲 + 本地文件；不引 Prometheus 客户端 | 单机产品，不需要暴露 `/metrics` | — |
| 配置 | 文件（TOML）+ 环境变量覆盖 | 人类可读 | YAML：缩进陷阱 |

### 4.2 数据库驱动

| 引擎 | 驱动 | CGO | 备注 |
|---|---|:--:|---|
| PostgreSQL | `jackc/pgx/v5`（原生协议，非 database/sql） | 否 | 需要 `CopyFrom`、`LISTEN/NOTIFY`、取消请求 |
| MySQL / MariaDB | `go-sql-driver/mysql` | 否 | 成熟稳定 |
| SQL Server | `microsoft/go-mssqldb` | 否 | 官方维护 |
| Oracle | `sijms/go-ora/v2` | 否 | **纯 Go**，避免 godror 的 OCI 依赖；若遇兼容问题再按 build tag 回退 godror |
| SQLite | `modernc.org/sqlite` | 否 | 转译版，性能低于 CGO 版但足够；大表扫描需 benchmark 确认 |
| Redis | `redis/go-redis/v9` | 否 | 官方 |
| MongoDB | `mongo-driver/v2` | 否 | 官方 |
| Elasticsearch | `elastic/go-elasticsearch/v8` | 否 | 官方 |

> **关于 Oracle**：go-ora 覆盖大部分场景，但高级特性（如 AQ、部分 LOB 流）不如 OCI 完整。策略是抽象出 `driver/oracle` 的两个实现，用 build tag 选择，默认纯 Go。需 POC 验证（见 Q2）。

### 4.3 SSH

`golang.org/x/crypto/ssh`，自实现隧道：

- 本地监听随机端口 → `ssh.Client.Dial` 到目标 host:port。
- 支持密码、私钥（含加密私钥）、`SSH_AUTH_SOCK` agent、跳板多跳（`ssh.Dial` 串联）。
- 隧道生命周期绑定连接，连接关闭即回收监听端口。
- 不做 `ProxyCommand` 外部进程调用（避免平台差异与安全面）。

### 4.4 前端

| 关注点 | 选择 | 理由 |
|---|---|---|
| 框架 | React 19 + TypeScript（strict） | 生态、招聘、Monaco 集成成熟 |
| 构建 | Vite | 快 |
| 编辑器 | **Monaco Editor** | SQL 高亮/补全 API 成熟；直接复用 VS Code 的 SQL 语法 |
| 表格 | TanStack Table v8 + TanStack Virtual | 无头设计，完全可控样式与虚拟化 |
| 状态 | Zustand（UI 态）+ TanStack Query（服务端态） | 避免把服务端缓存塞进全局 store |
| 样式 | Tailwind + Radix Primitives + 自建 design token | 无头组件 + 可访问性 |
| 图形 | 自绘 SVG（ER 图、计划树、阻塞链） | 避免重型图形库 |
| 测试 | Vitest + Playwright | — |

### 4.5 桌面壳

**选择：Wails v2**，理由见 [ADR-0001](adr/0001-tech-stack.md)。

关键点：因为核心是 loopback 服务、界面是纯 SPA，**壳是可替换的**。如果 Wails 在 macOS/Linux 的 CGO 打包出问题，换成 Tauri 或「托盘程序 + 系统浏览器」都只影响壳层，不影响内核与界面。

---

## 5. 模块划分

```
internal/
├─ model/             # 中立领域模型 + DTO，无逻辑
├─ api/               # HTTP/WS 路由、中间件、错误映射、OpenAPI 生成代码
├─ agent/             # 运行时、工具注册表、审批、上下文检索、provider 适配
├─ service/
│  ├─ connection/     # 连接生命周期、池、访问模式、隧道绑定
│  ├─ query/          # 执行、分页游标、取消、计划解析
│  ├─ metadata/       # 四层树、对象详情、缓存与失效
│  ├─ change/         # Change Plan 生成、风险分级、执行与回滚
│  ├─ migration/      # 迁移编排、类型映射、校验
│  ├─ monitor/        # 指标采集、慢查询、会话、锁
│  ├─ backup/         # 备份/恢复任务
│  ├─ audit/          # 审计事件写入与查询
│  └─ workspace/      # 工作区、历史、收藏
├─ driver/
│  ├─ connector.go    # 接口定义（唯一契约）
│  ├─ drivertest/     # 一致性测试套件
│  ├─ dialect/        # 标识符、分页、类型映射、计划解析
│  ├─ pg/ mysql/ mssql/ oracle/ sqlite/ redis/ mongo/ es/
├─ infra/
│  ├─ tunnel/         # SSH 隧道与跳板
│  ├─ secret/         # 凭据保管（OS keychain 封装）
│  ├─ metastore/      # SQLite 元数据 + 迁移
│  ├─ eventbus/       # 进程内事件 → WS 广播
│  └─ logging/        # slog 配置与脱敏
```

---

## 6. 关键数据流

### 6.1 一次查询（含流式结果）

> **下面的图是目标形态。v1 实际是同步 HTTP + JSON，没有 WebSocket、没有二进制分片**：`POST /api/v1/connections/{id}/query` 一次返回列元数据与首批行；后续用 `GET /api/v1/results/{cid}/page` 取页（游标 5 分钟 TTL，可续期），`?stream=1` 时改 NDJSON 逐条推。

```
UI ──POST /query──────────────────────────────► api
                                                 │ 校验 token / 连接 / 访问模式
                                                 ▼
                                              service/query.Execute
                                                 │ RiskGrade(sql, dialect) → R0
                                                 │ 取 Session（必要时开事务）
                                                 ▼
                                              driver.Connector.Query(ctx)
                                                 │
                     ┌───────────────────────────┴──────────────┐
                     │ 立即返回列元数据 + cursor_id              │
                     └───────────────────────────┬──────────────┘
                                                 ▼
                                 WS event: result.columns
UI ◄──────────────────────────────────────────────
UI ──GET /result/{cursor}/page?keyset=...──────► 取页，缓存 3 页窗口
UI ◄──────────── rows + next_cursor ─────────────
                                                 │
                                              审计事件写入（结束时）
```

要点：

- **列元数据先到**，UI 可以立刻画出表头与骨架，不用等数据。
- 分页由**客户端驱动**，服务端持有游标直到关闭或超时（默认 5 分钟，可续期）。
- 结果分片用紧凑格式（列式压缩或行式二进制 + 字符串字典），不走 JSON；仅在调试模式下开 JSON。

### 6.2 一次变更（Change Plan）

```
UI/Agent ──POST /changes/plan──► change.Plan
                                   │ 1. 计算目标 DDL/DML
                                   │ 2. 影响预估（行数、锁、依赖对象）
                                   │ 3. 生成回滚语句
                                   │ 4. 风险分级
                                   ▼
                              ChangePlan (draft)
UI ◄──────── plan + 风险 + 回滚 ──┘
UI ──POST /changes/{id}/apply────► 校验审批凭证 → 执行 → 审计
```

**`Plan` 与 `Apply` 必须分离**：这是所有安全性的落点。Agent 也一样——它可以自主生成 plan，但 apply 需要授权。

### 6.3 会话与事务

- `Connection` 持有 `*sql.DB` / 原生池。
- **Session** = 一次逻辑上下文，默认从池借一条物理连接，语句结束即还。
- 一旦 `BEGIN`，Session **钉住**该物理连接直到 `COMMIT/ROLLBACK` 或超时；期间其他标签不可借用。
- 事务状态在服务端持有，不信任前端传参（避免「前端说是只读但实际在事务里写」）。

---

## 7. 并发、取消与超时

| 关注点 | 方案 |
|---|---|
| 取消查询 | `context` 贯穿；各驱动映射为引擎侧取消（PG: cancel request；MySQL: 独立连接发 `KILL QUERY`；MSSQL: attention；Oracle: 断开或 `KILL SESSION` 兜底） |
| 超时 | 三级：连接超时（默认 10s）、语句超时（默认无限制，可配）、游标存活期（5min） |
| 背压 | WS 发送队列有界（默认 8 页）；溢出即暂停拉取并提示 |
| 并发上限 | 每连接可并发语句数（默认 4，事务期 1） |
| 长任务 | 备份/迁移走任务队列，进度经 WS 推送，可暂停/取消 |
| 进程退出 | 优雅关闭：先停接受新请求，再取消在途 ctx，最后关池；写操作不允许中断在提交点 |

---

## 8. 缓存策略

| 缓存 | 位置 | 失效 |
|---|---|---|
| 元数据（对象清单/详情） | 内存 LRU（每连接） | 手动刷新、检测到 DDL 后失效、TTL 5min |
| 库画像 | 持久化到 SQLite（带时间戳） | 手动重算 |
| 结果页 | 内存 LRU（每游标 3 页窗口） | 游标关闭 |
| schema 向量索引（Agent 用） | SQLite（向量以 BLOB 存）+ 轻量检索 | 元数据变更后增量更新 |
| 类型映射表 | 编译期常量 | — |

**不做**跨进程共享缓存：单机产品，收益不抵复杂度。

---

## 9. 扩展点

按「加一个新引擎要改哪些文件」检验架构：

1. 实现 `driver.Connector`（含 `Capabilities()` 与 `Target()`——后者是「确认你连的是哪个库」用的目标标识，见 [ADR-0015](adr/0015-connector-target-and-restore-confirmation.md)）。
2. 在 `driver/dialect/` 加方言（引用、分页、类型映射、计划解析）。
3. 在 `driver/drivertest/cases.go` 注册该引擎的期望能力。
4. 前端无需改动——UI 依据 `Capabilities()` 渲染与降级。

若第 4 步被迫改动，说明能力抽象不足，应先修抽象再继续。

---

## 10. 错误模型

三层转换，越往上越「人话」：

```
driver 错误（引擎原生码 + 文本）
   → service 错误（分类：连接/认证/权限/语法/约束/超时/取消/资源）
      → api 错误（{code, category, message, hint, engine_detail, retryable}）
```

- `code` 稳定可被前端与 Agent 判断（如 `DB_AUTH_FAILED`）。
- `hint` 给可执行建议（如「该实例要求 TLS，请在连接设置中启用」）。
- `engine_detail` 保留原生错误码便于排障，但**已脱敏**。

详见 [`12-api-contract.md`](12-api-contract.md#4-错误模型)。

---

## 11. 部署形态

| 场景 | 形态 | 说明 |
|---|---|---|
| 个人 | 桌面安装包 | 内嵌核心，无外部依赖 |
| 团队 | `dbrover serve` 容器 | 需要认证与多用户（v1.1） |
| 远程开发机 | `serve` + SSH 端口转发 | 复用已有隧道能力 |
| CI | `dbrover exec` | 无头执行 + 断言退出码 |

---

## 12. 待决策项

见 [`13-roadmap.md`](13-roadmap.md#6-待决策) 与 `docs/adr/`。当前列出会影响本文档的项：

- Oracle 驱动最终选型（go-ora vs godror）
- 桌面壳最终选型（Wails vs Tauri）
- 团队模式的认证方案（本地账号 vs OIDC）
