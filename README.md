# DBRover

> 一个以 **AI Agent 为第一公民**的跨引擎数据库工作台。把「连接 → 查看 → 改数据 → 改结构 → 迁移 → 排障」收进一个可审计、可授权、可回滚的工作流里。

DBRover 面向同时维护多种数据库的工程师：一个人可能上午在 MySQL 上追一个慢查询，下午在 PostgreSQL 上做一次表结构变更，晚上去 Redis 里清一批脏 key，还要给老板解释为什么昨晚的 MongoDB 备份没跑完。

传统工具把每种数据库做成一个独立窗口，DBRover 把**引擎差异收敛到连接器层**，让上层的编辑器、数据网格、结构设计器、迁移器、监控面板、AI Agent 只面向一套统一语义。

---

## 1. 定位与取舍

| 维度 | DBRover 的选择 | 反面 |
|---|---|---|
| 产品形态 | 桌面优先（单机零部署），同一份核心支持 `serve` 模式给团队 | 只做 SaaS，先解决部署再解决体验 |
| 引擎覆盖 | 关系库做深（PG/MySQL/SQLServer/Oracle/SQLite），NoSQL 做「够用的运维视角」 | 每种库都浅尝辄止 |
| AI 定位 | Agent 是一等公民：直接调用与人工操作**走同一套受控 API** | 聊天框里吐出 SQL 让你自己复制粘贴 |
| 安全基线 | **默认只读**，写操作按风险分级审批，全链路审计 | 连上就有全部权限 |
| 环境感知 | 区分 `production`/`staging`/`test`/`local`，**确认强度随环境分级**：测试库改数据不打扰，生产库逐步加摩擦 | 所有库一视同仁，要么处处弹窗、要么处处裸奔 |
| 变更安全 | DDL/DML 变更先出 `plan`（含影响行数预估与回滚语句），确认后执行 | 点一下「保存」直接 ALTER |
| 技术形态 | Go 单二进制核心（免 CGO 交叉编译）+ Web 技术栈 UI | JVM/.NET 重运行时 |

四条不可妥协的原则，写进所有后续设计：

1. **单一能力源**：UI 和 Agent 不允许绕过 `service` 层直连数据库。任何能力先有 API，再有界面，再有工具。
2. **默认最小权限**：连接创建时即声明环境（生产/预发/测试）与访问模式（`read-only` / `read-write` / `admin`），默认按最严处理，只读连接在协议层和 SQL 分析层双重拦截写操作。
3. **可回放**：每一次执行（人发起的、Agent 发起的）都落一条审计事件，包含 SQL、参数、耗时、影响行数、连接、发起者、审批记录。
4. **放宽需显式声明，收紧可自动推断**：任何降低保护的动作（把生产改标成测试、把只读改成读写）必须由人显式操作并留痕；任何提高保护的判断（检测到可能是生产）可以自动生效。默认值的代价必须落在「麻烦」一侧，不能落在「事故」一侧。

---

## 2. 引擎能力总览

`●` 完整支持 · `◐` 部分支持（能力降级） · `○` 不适用

| 能力 | PostgreSQL | MySQL/MariaDB | SQL Server | Oracle | SQLite | Redis | MongoDB | Elasticsearch |
|---|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|
| SQL 编辑器 | ● | ● | ● | ● | ● | ◐ 命令 | ◐ MQL | ◐ DSL |
| 结果集网格 | ● | ● | ● | ● | ● | ◐ 类型化视图 | ◐ 文档树 | ◐ 命中列表 |
| 数据编辑 | ● | ● | ● | ● | ● | ● | ● | ◐ 单文档 |
| 结构管理 | ● | ● | ● | ● | ◐ | ○ | ◐ 集合/校验器 | ◐ 映射 |
| 索引管理 | ● | ● | ● | ● | ● | ○ | ● | ◐ 设置 |
| 视图/存储过程 | ● | ● | ● | ● | ◐ 视图 ●/过程 ○ | ○ | ◐ 视图 | ○ |
| 执行计划 | ● | ● | ● | ◐ | ● | ○ | ◐ | ◐ |
| 慢查询分析 | ● | ● | ● | ◐ | ○ | ● | ◐ | ● |
| 会话管理/Kill | ● | ● | ● | ● | ○ | ● | ● | ◐ |
| 备份/恢复 | ● | ● | ● | ◐ | ● | ● | ● | ● |
| 跨库迁移 | ● | ● | ● | ● | ● | ◐ | ◐ | ○ |
| SSH 隧道 | ● | ● | ● | ● | ○ | ● | ● | ● |

> 完整的逐项支持矩阵、降级策略与方言差异见 [`docs/04-connectors.md`](docs/04-connectors.md)。

---

## 3. 架构一览

```
┌──────────────────────────────────────────────────────────────────┐
│  UI  React 19 · TypeScript · Vite · Monaco · TanStack            │
│  SQL工作台 │ 数据编辑 │ 结构设计 │ 迁移 │ 监控排障 │ AI 助手 │ 连接 │
└──────────────────────────┬───────────────────────────────────────┘
                           │  loopback HTTP/JSON + WebSocket 事件流
                           │  (每会话随机 token，不监听公网)
┌──────────────────────────▼───────────────────────────────────────┐
│  dbrover-core  (Go, 免 CGO)                                       │
│                                                                   │
│  api/      REST + WS，OpenAPI 契约，事件推送                       │
│  service/  连接池 · 查询执行 · 元数据缓存 · 变更编排 · 监控 · 审计  │
│  agent/    工具注册表 · 权限分级 · 计划-执行-观察循环 · 上下文检索   │
│  driver/   Connector 接口 + 8 引擎实现 + 方言层                     │
│  infra/    SSH 隧道 · 凭据保管 · SQLite 元数据库 · 日志与指标       │
└──────────────────────────────────────────────────────────────────┘
        ▲                    ▲                        ▲
        │                    │                        │
   Wails/WebView2     dbrover serve              MCP 客户端
   (桌面壳)            (团队/容器)              (外部 Agent)
```

关键约束：

- 核心是**可独立运行的服务**，桌面壳只是一层窗口。因此团队模式、容器部署、CI 中的无头校验都能复用同一份实现。
- 元数据（连接配置、历史、收藏、审计）落在本地 SQLite；**密码永远不进 SQLite**，只存 OS 凭据库中的句柄。
- Agent 与人工共用同一套 `service` 方法，权限判断只在一个地方做。

---

## 4. 文档导航

| 文档 | 内容 | 读者 |
|---|---|---|
| [00-glossary](docs/00-glossary.md) | 术语表、概念层级（server/catalog/schema/object） | 全体 |
| [01-product-requirements](docs/01-product-requirements.md) | 角色、场景、需求池、验收口径、竞品差异 | 产品 / 设计 |
| [02-architecture](docs/02-architecture.md) | 分层、进程模型、技术选型与理由、扩展点 | 研发 |
| [03-domain-model](docs/03-domain-model.md) | 领域实体、状态机、元数据模型、存储结构 | 研发 |
| [04-connectors](docs/04-connectors.md) | 连接器契约、能力矩阵、方言层、降级策略 | 研发 |
| [05-sql-workspace](docs/05-sql-workspace.md) | 编辑器、补全、执行、结果集、执行计划 | 研发 / 设计 |
| [06-data-editor](docs/06-data-editor.md) | 数据网格、编辑提交语义、批量修改、生成数据 | 研发 |
| [07-schema-designer](docs/07-schema-designer.md) | 表/索引/视图/例程设计、DDL 差异比对与安全应用 | 研发 |
| [08-backup-and-migration](docs/08-backup-and-migration.md) | 备份策略、恢复、跨引擎迁移、校验 | 研发 / 运维 |
| [09-observability](docs/09-observability.md) | 性能面板、慢 SQL、会话、锁与阻塞 | 研发 / 运维 |
| [10-connection-and-security](docs/10-connection-and-security.md) | 连接管理、SSH 隧道/跳板、凭据、审计、只读守卫 | 研发 / 安全 |
| [11-ai-agents](docs/11-ai-agents.md) | Agent 运行时、工具面、审批模型、上下文检索、评测 | 研发 |
| [12-api-contract](docs/12-api-contract.md) | REST/WS/MCP 契约与错误模型 | 研发 / 集成方 |
| [13-roadmap](docs/13-roadmap.md) | 里程碑、版本范围、验收门禁 | 全体 |
| [adr/](docs/adr/) | 架构决策记录 | 研发 |

---

## 5. 当前状态

**阶段：M2 完成，M3 切片一、二完成，备份与迁移已落地**。查询、数据编辑、结构设计、导出、结构比对、备份恢复、跨引擎迁移、Agent 都可用；SQL Server / Oracle 与 NoSQL 引擎未做（卡在镜像与实例）。

| 模块 | 状态 |
|---|---|
| `internal/model` | 领域模型、能力声明（含 `IndexValidity` / `AlterColumnNullability` / `AlterColumnDefault` / `AddCheck` / `AddTrigger` / `Sequences` / `MaterializedViews` / `UserTypes` / `Routines` / `AddRoutine`）、环境严格度、元数据模型 |
| `internal/driver/dialect` | 5 种 SQL 方言：词法、语句切分（含 dollar-quote、`DELIMITER`、`GO`、例程/触发器体）、指纹、分类、条件分析、标识符引用，以及改列类型/可空/默认值、索引、外键、检查约束、视图/物化视图、序列、触发器、枚举类型、例程（新建与整段替换）的语句渲染 |
| `internal/policy` | 风险分级 `R0`–`R4`、摩擦分级 `F0`–`F5` |
| `internal/driver` | Connector 契约（含 `Target()`：目标自报身份）+ `drivertest` 一致性套件（19 项，含表注释、声明长度、索引有效性、触发器/序列/类型/例程列表——例程那条还会把取到的定义回放一遍） |
| `internal/driver/sqlite` `mysql` `pg` | 三个引擎完整实现，真机验收（SQLite / MySQL 5.7 / PostgreSQL 17） |
| `internal/service` | 执行门；变更集与预案（乐观并发、回滚、一键撤销）；结构变更（建表/加列/删列/改列类型/可空/默认值/索引/外键/检查约束/视图/物化视图/序列/触发器/枚举类型/例程新建与整段替换）+ 收窄预检、孤儿预检、依赖阻断、等锁阻断；批量修改（分批、NDJSON 进度、可中止）；流式导出（CSV / NDJSON / SQL / Markdown / XLSX）；游标与键集分页；结构比对（含物化视图，一侧引擎没有这个概念时只记 `need_review`）；**备份**（容器 v1、gzip、AES-256-GCM、表级续跑）与**恢复**（同引擎、单事务、报告；目标非空时要写出目标名才允许覆盖）；**跨引擎迁移**（预检、建表、键集搬数据、行数+摘要校验、检查点续跑、外键与注释补建） |
| `internal/api` | 回环 HTTP：token 鉴权、连接、`exec`/`query`（NDJSON 流）、键集分页、变更与结构预案、批量修改、外键预检、结构比对、导出多格式、备份/恢复/迁移端点、审计查询、`/agent/*`（会话、消息流、审批、工具清单、中止、压缩、导出）、OpenAPI 契约 |
| `internal/agent` | Provider 抽象（OpenAI 兼容）、14 个工具、权限三模式与环境上限、执行循环（步数/时长/循环/失败上限）、审批门（预案只由服务端保管）、三路上下文召回、长会话压缩、评测集 |
| `internal/infra/tunnel` | SSH 隧道与跳板链：多跳、密码/私钥/agent 认证、主机密钥 TOFU |
| `internal/webui` | **临时界面**：免构建的内嵌 Web UI（连接、对象树、SQL、结果网格、字段编辑、筛选排序、结构变更与比对、批量修改、导出、审批弹窗、Agent 面板） |
| `cmd/dbrover` | CLI：`version` / `health` / `tables` / `exec` / `serve`（`-open`、`-audit-anchor`）/ `tunnel` / `secret` / **`backup create|inspect`** / **`restore`** / **`migrate plan|run`**（`-probe` 按实际值判长度） |

> `internal/webui` 是为了「能点着验收」而做的最小实现，**不是最终前端**。最终前端仍是 [ADR-0001](docs/adr/0001-tech-stack.md) 规定的 React + TypeScript + Vite 栈；先做无构建版本是因为本机磁盘紧张，不装 node_modules。

> 跨引擎差异的写法约定见 [ADR-0005](docs/adr/0005-engine-differences-as-capabilities.md)：能力有无进 `Capabilities`，语句文本归 `dialect`，其余位置不得判断引擎名。
>
> 删除前的依赖查询放在驱动层，精确与近似的边界由驱动自己标注（`Dependent.Exact`），见 [ADR-0006](docs/adr/0006-dependency-check-in-driver.md)。
>
> 非关系型引擎（Redis / Mongo / ES）不实现 `Connector`/`Metadata`，另用 `KVConnector` 一族接口，`api` 靠类型断言而不是引擎名选路径，见 [ADR-0007](docs/adr/0007-nosql-connector-model.md)。

**尚未实现**：React 正式前端、桌面壳（Wails，待拍板）、SQL Server / Oracle（镜像拉不下来，见 [ADR-0008](docs/adr/0008-mssql-connector.md)）、MongoDB / Elasticsearch、引擎特有项（`EngineExtra`）表单、磁盘余量探测（只有 SQLite 能诚实实现——MySQL/PG 拿不到所在卷余量）、例程依赖检查、手写 SQL 反解回表单、Agent 的向量召回与 `/agent/providers`、测试数据生成。


## 6. 快速开始

```powershell
go build -o bin/dbrover.exe ./cmd/dbrover

bin/dbrover.exe version
bin/dbrover.exe exec -db demo.db -env local -sql "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"
bin/dbrover.exe exec -db demo.db -env local -sql "INSERT INTO t (id, v) VALUES (1, 'a')"

# 生产环境：批量更新需要确认，退出码 2
bin/dbrover.exe exec -db demo.db -env production -sql "UPDATE t SET v='b' WHERE id > 0"
bin/dbrover.exe exec -db demo.db -env production -approve -sql "UPDATE t SET v='b' WHERE id > 0"

# 只读连接：写入被双层拦截
bin/dbrover.exe exec -db demo.db -ro -env test -sql "DELETE FROM t"

# 起服务（浏览器界面 + HTTP API），审计落盘
bin/dbrover.exe serve -addr 127.0.0.1:7788 -audit ./audit.jsonl

# 备份与恢复（容器格式：头 + 每张表一段 SQL；可选 gzip / AES-256-GCM）
bin/dbrover.exe backup create -db demo.db -env local -out demo.dbr -compress
bin/dbrover.exe backup inspect demo.dbr          # 只读头，不连库
bin/dbrover.exe restore -file demo.dbr -db dst.db -env local -yes

# 迁移一张表（同引擎或跨引擎）：预检 → 建表 → 搬数据 → 校验
bin/dbrover.exe migrate plan -db demo.db -table t -to-db target.db
bin/dbrover.exe migrate run  -db demo.db -table t -to-db target.db -yes
# 跨引擎时给目标端：-to-engine postgres -to-host H -to-port 5432 -to-user U -to-db D -to-password-stdin
# 声明长度收窄会被拦；加 -probe 读源表实际最大长度，放得下才放行
bin/dbrover.exe migrate plan -engine mysql -host H -user U -db S -table t -to-engine postgres -to-host H2 -to-user U2 -to-db D2 -probe

# 打开 serve 打印的 http://127.0.0.1:7788/#t=<token> 即可操作
```

## 7. 开发与验证

```powershell
pwsh -File scripts/check.ps1     # gofmt + vet + build + test + test -race（不依赖真机）

# 有真机时跑全量（连接器一致性套件、跨引擎迁移、结构变更真机用例都在内）
# MySQL 实例必须是 utf8mb4——套件刻意写中文与 emoji 验类型保真，latin1 下报 1366
$env:DBRover_TEST_MYSQL='root:testpass@HOST:13306'
$env:DBRover_TEST_PG='postgres:testpass@HOST:15432'
go test ./... -count=1 -timeout 1800s

# 端到端验收脚本（自己起 serve、跑真实引擎、失败即红）
pwsh -File scripts/accept/bulk-20k-sqlite.ps1
pwsh -File scripts/accept/compare-cross.ps1
```

要求 Go 1.26+（`modernc.org/sqlite` 的最低版本）。`CGO_ENABLED=0`，全部依赖纯 Go。

真机套件在**空实例**上可直接跑：用例会自己建 `dbrover_test`。验收脚本的跑法与排错顺序见 [`scripts/accept/README.md`](scripts/accept/README.md)。

约定见 [`AGENTS.md`](AGENTS.md)：文档先行、破坏性变更落 ADR、跨平台产物走 CI。

## 8. 许可证

待定（预计 Apache-2.0）。
