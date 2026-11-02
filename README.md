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
| 变更安全 | DDL/DML 变更先出 `plan`（含影响行数预估与回滚语句），确认后执行 | 点一下「保存」直接 ALTER |
| 技术形态 | Go 单二进制核心（免 CGO 交叉编译）+ Web 技术栈 UI | JVM/.NET 重运行时 |

三条不可妥协的原则，写进所有后续设计：

1. **单一能力源**：UI 和 Agent 不允许绕过 `service` 层直连数据库。任何能力先有 API，再有界面，再有工具。
2. **默认最小权限**：连接创建时即声明 `read-only` / `read-write` / `admin`，只读连接在协议层和 SQL 分析层双重拦截写操作。
3. **可回放**：每一次执行（人发起的、Agent 发起的）都落一条审计事件，包含 SQL、参数、耗时、影响行数、连接、发起者、审批记录。

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
| 视图/存储过程 | ● | ● | ● | ● | ◐ | ○ | ◐ 视图 | ○ |
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

**阶段：文档基线（M0）**。尚无代码。

接下来按 [`docs/13-roadmap.md`](docs/13-roadmap.md) 的 M1 里程碑推进：先打通「PostgreSQL + MySQL 的连接、元数据、查询、结果集」最小闭环，再叠加写入与 AI。

---

## 6. 开发约定

见 [`AGENTS.md`](AGENTS.md)。要点：

- 文档先行：任何新能力先在对应 `docs/` 里写清契约与验收，再写代码。
- 破坏性设计变更必须落 ADR。
- 跨平台产物走 CI（GitHub Actions tag 触发），本地只跑静态检查与单测，不做重型构建。

## 7. 许可证

待定（预计 Apache-2.0）。
