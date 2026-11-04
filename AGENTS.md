# AGENTS.md — DBRover 协作约定

本文件是**所有协作者（人 / AI Agent）**进入本仓库时的入口。先读完再动手。

---

## 1. 项目一句话

DBRover 是一个以 AI Agent 为一等公民的跨引擎数据库工作台，Go 核心 + Web 技术栈 UI，桌面优先、可服务化。

---

## 2. 文档先行（强制）

本仓库是**文档驱动**的。顺序永远是：

```
docs/*.md 契约与验收  →  ADR（若涉及取舍）  →  代码  →  测试  →  文档回填
```

- 新增能力：先在对应 `docs/` 文件里补「能力契约 + 验收口径 + 降级行为」，再实现。
- 改动已有能力：同步更新受影响文档；文档与实现不一致视为缺陷。
- 涉及以下任一项，必须新增 ADR（`docs/adr/NNNN-<slug>.md`）：技术栈、进程模型、跨模块接口、安全边界、存储格式、引入新依赖。

---

## 3. 目录结构

```
DBRover/
├─ README.md
├─ AGENTS.md
├─ go.mod                 # Go 1.26+，CGO_ENABLED=0
├─ docs/                  # 设计与契约，中文
│  └─ adr/
├─ cmd/
│  ├─ dbrover/            # CLI / serve 入口（已实现 version/health/tables/exec）
│  └─ dbrover-desktop/    # 桌面壳入口（待实现，Wails）
├─ internal/
│  ├─ api/                # HTTP/WS 层，只做协议转换（待实现）
│  ├─ service/            # 业务编排，权限判断唯一位置（执行门已实现）
│  ├─ driver/             # Connector 接口与各引擎实现
│  │  ├─ connector.go     # 接口契约
│  │  ├─ drivertest/      # 一致性测试套件
│  │  ├─ dialect/         # 方言：词法、切分、指纹、分类、引用
│  │  ├─ sqlite/          # 已实现
│  │  └─ pg/ mysql/ mssql/ oracle/ redis/ mongo/ es/   # 待实现
│  ├─ policy/             # 风险分级 R0–R4、摩擦分级 F0–F5
│  ├─ agent/              # Agent 运行时：Provider、工具面、执行循环（切片一已实现）
│  ├─ infra/              # SSH 隧道、凭据库、元数据存储（待实现）
│  └─ model/              # 领域模型与 DTO
├─ web/                   # 前端（待实现）
├─ scripts/check.ps1      # 本地质量检查
├─ testdata/              # 各引擎的 docker-compose 与 fixture
└─ .github/workflows/     # CI，仅 tag 触发打包
```

**分层铁律**：`api → service → driver → 物理连接`，禁止越层与反向依赖。`driver` 不得 import `service`，`agent` 只能通过 `service` 触达数据。

**当前依赖方向**（可被 `go list` 验证）：

```
cmd/dbrover → service → {driver, driver/sqlite, policy}
agent → {service, policy, model}          # 与 api 并列，只经 service 触达数据
api → {service, agent, driver, policy, model}
driver/sqlite → {driver, driver/dialect, model}
policy → {driver/dialect, model}
dialect → model
model → （无）
```

---

## 4. 代码规范

### Go
- `gofmt` + `go vet` 必须干净；`golangci-lint` 按仓库配置。
- 错误统一 `%w` 包装，API 层映射为结构化错误码（见 `docs/12-api-contract.md`）。
- 所有对外方法接收 `context.Context`，并遵守取消语义。
- **禁止在 `driver` 层做重试以外的业务决策**；重试策略集中在 `service`。
- SQL 一律参数化；拼接只允许发生在「标识符引用」场景，且必须经 `dialect.QuoteIdent`。
- 新增引擎实现必须通过「连接器一致性测试套件」（`driver/drivertest`），不允许只写 happy path。

### 前端
- TypeScript strict，禁用 `any` 逃逸（必要处写 `unknown` + 类型守卫）。
- 数据获取统一走生成的 API client（来自 OpenAPI），不手写 `fetch`。
- 组件不得直接持有连接配置；一切经 store 与 API。

### 通用
- 无 TODO 进主干。需要延后必须建 issue 并在文档标注「已知限制」。
- 不引入需要本地 CGO 的依赖（除非 ADR 批准）：这是免 CGO 交叉编译的前提。
- 保留文件原有换行风格；新文件用 LF。

---

## 5. 测试与验证

| 层次 | 手段 | 门禁 |
|---|---|---|
| 单元 | `go test ./...` | 必须绿 |
| 连接器一致性 | `testdata/docker-compose.*` 起真实实例，跑 `drivertest` 套件 | 目标引擎全绿 |
| 契约 | OpenAPI 快照比对，破坏性变更需 ADR | 必须无未批准 diff |
| 前端 | `vitest` + Playwright 冒烟 | 主流程全绿 |
| 静态 | `gofmt -l`、`go vet`、`golangci-lint`、`tsc --noEmit` | 零输出 |
| 文档一致性 | `internal/ci`：文档里引用的 `Test*`、`(NNNN-*.md)` ADR 链接、`scripts/*.ps1`，以及 `docs/12` 里标 ✅ 的端点，逐个核对 | 零悬空（改名/删除后必须同步文档） |

**本地只跑静态检查与单测**；完整矩阵与产物构建交给 CI。

本地检查入口（发版前必须全绿）：

```powershell
pwsh -File scripts/check.ps1     # gofmt + vet + build + test + test -race
pwsh -File scripts/check.ps1 -TmpDir $env:LOCALAPPDATA\Temp   # 临时目录所在盘快满时用这个
```

要求 Go 1.26+、`CGO_ENABLED=0`。新增依赖前先确认它是纯 Go。

**磁盘前置检查**：脚本会先看临时目录所在盘的可用空间，低于 2 GB 直接拒绝并给出改法。原因是一次真实的误判——盘满时链接阶段报 `There is not enough space on the disk`，读起来像代码错误，实际上是磁盘问题（`F:` 曾 0 字节可用，换 `-TmpDir` 到 `C:` 后立刻全绿）。

---

## 6. CI / 发版

- 打包发版**只由 tag 触发**（`v*` / `desktop-v*`），PR 只跑测试，不启动打包 runner。
- 一条 tag 只应产生一条发布流水线，并发组固定不按事件区分。
- 发版前的质量检查（格式、vet、单测、README/文档与代码一致性、CHANGELOG）**在本地完成**，不塞进 release job。
- 产物：`dbrover`（多平台 CLI/serve）、`dbrover-desktop`（安装包）、容器镜像。

**对应文件**

| 文件 | 触发 | 职责 |
|---|---|---|
| `.github/workflows/ci.yml` | `pull_request`、push `main` | 只跑 gofmt / vet / build / test / test -race + 前端脚本文法检查 |
| `.github/workflows/release.yml` | push tag `v*` | 只做 `CGO_ENABLED=0` 交叉编译（5 个平台）与发布 |

版本号通过 `-ldflags "-X main.version=<tag>"` 注入，因此 `cmd/dbrover` 里的 `version` 必须是 `var` 而不是 `const`。

**这些规则由测试强制**：`internal/ci` 会检查

- `release.yml` 不含 `pull_request` / `branches:`；
- 并发组固定为 `release`，不引用 `github.event_name`；
- `release.yml` 里不出现 `go test` / `gofmt` / `go vet`；
- 全仓库只有一条发版路径（含 `action-gh-release` 或 `goreleaser` 的文件只能有一个）。

违规会让 `go test ./...` 直接失败，不依赖人肉 review YAML。

---

## 7. 安全红线

1. **凭据永不落明文**：密码/私钥/口令只存 OS 凭据库，日志与错误信息中一律脱敏。
2. **默认只读**：新建连接默认 `read-only`；提升权限需显式操作并记录审计。
3. **生产库禁止隐式写**：Agent 发起的任何写操作必须经过风险分级与（按配置）人工确认。
4. **不记录敏感数据**：查询历史默认记录 SQL 文本但不记录结果集内容；开启「记录结果」需用户显式同意。
5. **回环优先**：本地服务只监听 `127.0.0.1`，会话 token 每次启动重生成。

---

## 8. 文档写作口径

- 中文为主，专有名词、API 名、字段名保留英文原形。
- 需求带稳定编号（`FR-*` / `NFR-*`），便于在 issue、测试、ADR 中引用。
- 每个能力描述必须包含：**做什么 / 怎么触发 / 失败与降级 / 验收**。
- 不写「优化了体验」这类空话；写可被验证的行为。

---

## 9. 给 AI 协作者的行为约束

- 修改前先读相关 `docs/` 文件与 ADR，不要凭直觉补设计。
- 不确定引擎行为时，查官方文档或起容器实测，不要猜。
- 任何会影响用户数据（DDL/DML/迁移）的实现，先在文档里写清「影响面估计 + 回滚路径」。
- 不要把「测试通过」当作「功能正确」：连接器类改动必须有真实实例验证记录。
