# ADR-0007 · 非关系型引擎单独一套连接器接口，不硬塞进 `Metadata`

- 状态：**已采纳**
- 日期：2026-11-03
- 影响面：`internal/driver`（新增接口族）、`internal/service`（新增 KV 执行路径）、`internal/policy`（命令风险表）、`internal/api`（`/kv/*` 端点）、`internal/webui`

---

## 背景

接下来要接 Redis（`docs/04` §5.6 已有行为约定），后面还有 Mongo 与 ES。它们与关系型引擎的差别不是「细节不同」，而是**没有共用的概念**：

| 关系型概念 | Redis 里对应什么 |
|---|---|
| Catalog / Schema | 逻辑库（0–15，仅非集群模式） |
| Table | 没有。数据是 key → 类型化的值 |
| Column | hash 有 field，其他类型没有「列」 |
| Primary key | key 本身就是标识 |
| SQL 语句 | 命令（`GET` / `HSET` / `ZRANGE` …），有命令族与参数语法 |
| 方言层（`dialect`） | 不适用：没有词法、没有标识符引用、没有 WHERE |

现有接口 `driver.Metadata` 有 `Objects` / `DescribeTable` / `DDL` / `Dependents` / `Blockers`。硬把 Redis 塞进去，只会得到一堆语义空洞的实现：`DescribeTable("user:1")` 是什么？`DDL` 返回什么？`Dependents` 永远空？这些「看起来实现了」的空壳是最糟的——它会让上层以为能力存在。

同时也不能在 `api` 里 `switch engine { case EngineRedis }`：ADR-0005 已经把这个方向否掉了，而且它会让每加一个 NoSQL 引擎都要改 `api`。

---

## 决策

### 1. 关系型与非关系型各一套接口族，不合并

```go
// 关系型（已有，不动）
type Connector interface { Engine(); Capabilities(); Health(); Acquire(); Meta(); ... }
type Metadata interface { Objects(); DescribeTable(); DDL(); Dependents(); Blockers(); ... }

// 非关系型（新增）
// KVConnector 由 key-value 引擎实现。它是独立接口，不实现 Connector。
type KVConnector interface {
    Engine() model.Engine
    Capabilities() model.Capabilities
    Health(ctx) (*driver.HealthInfo, error)
    Open(ctx, driver.ConnectConfig) error
    Close() error

    // Scan 按游标分批遍历 key。禁止实现成 KEYS —— 集群模式下没有 KEYS，
    // 单机模式下它也会阻塞整个实例（docs/04 §5.6）。
    Scan(ctx, opt KVScanOptions) (*KVPage, error)
    // Inspect 读一个 key 的类型、TTL、编码、元素数与体积。
    Inspect(ctx, key string) (*KVEntry, error)
    // Value 按类型分页读值（HSCAN / SSCAN / ZRANGE / XRANGE / GETRANGE）。
    Value(ctx, key string, opt KVValueOptions) (*KVValue, error)
    // Mutate 按类型写值；写操作同样要过执行门与审计。
    Mutate(ctx, op KVMutation) (*KVResult, error)
    // Command 是「命令模式」的逃生口：结构化命令 + 参数，不是自由文本。
    Command(ctx, name string, args []any) (*KVResult, error)
}
```

**判据**：`Connector`/`Metadata` 描述的是「表、列、SQL」这套关系型语义；凡是无法在关系型语义下**有意义**地表达的，就另开接口，而不是留一个永远返回空的方法。

### 2. 「用哪个接口」由类型断言决定，不由引擎名判断

```go
// api 层
if kv, ok := e.drv.(driver.KVConnector); ok { ... }   // 非关系型
else { ... }                                          // 关系型
```

这与 `docs/04` §2 已有的「两层判断」一致：`Capabilities()` 给 UI 决定要不要显示，类型断言给代码决定能不能调。新增引擎时，`api` 一行都不用改。

### 3. 执行门、审计、风险分级复用，但风险表分开

`service` 里新增 `KVExecutor`，与 `Executor` 共用：

- 同一个 `AuditSink`（审计字段一致：actor / action / risk / friction / outcome / duration）；
- 同一套摩擦分级 `policy.FrictionFor`（环境 × 风险 → F0–F5）；
- 同一套审批凭证（绑定「命令指纹」而不是 SQL 指纹）。

**风险表必须分开**：`policy` 里的 SQL 分级依赖 `dialect` 的语句分类，Redis 命令没有语句结构。新增 `policy.redisRisk`：

| 命令族 | 风险 | 说明 |
|---|:--:|---|
| `GET` `MGET` `EXISTS` `TTL` `TYPE` `STRLEN` `HLEN` `LLEN` `SCARD` `ZCARD` `XLEN` `HGET` `HGETALL` `LRANGE` `SMEMBERS` `ZRANGE` `XRANGE` | `R0` | 只读 |
| `SCAN` `HSCAN` `SSCAN` `ZSCAN` `MEMORY USAGE` | `R1` | 只读但有遍历成本，大 key 上会慢 |
| `SET` `HSET` `LPUSH` `SADD` `ZADD` `XADD` `EXPIRE` | `R2` | 单 key 写 |
| `DEL` `UNLINK` `RENAME` `RENAMENX` `PERSIST` | `R2` | 单 key 删除/改名；`UNLINK` 优先（非阻塞），但风险同级 |
| `FLUSHDB` `FLUSHALL` `SWAPDB` | `R4` | 整库/整实例清空 |
| `KEYS` | `R3` | 会阻塞实例；UI 不提供，命令模式里命中要提示改用 `SCAN` |
| `CONFIG SET` `DEBUG` `SHUTDOWN` `SCRIPT` `EVAL` | `R4` | 改变实例行为或执行任意代码 |
| `CLIENT KILL` `REPLICAOF` `SLAVEOF` | `R4` | 影响别人或拓扑 |

不在表里的命令一律按 `R4` 处理（未知即最危险），并要求审批——与「`unknown` 环境等同生产」同一条原则。

### 4. HTTP 端点用通用路径，不带引擎名

```
GET  /api/v1/connections/{id}/kv/keys?cursor&pattern&type&count
GET  /api/v1/connections/{id}/kv/keys/{key}            # 类型 + TTL + 元素数
GET  /api/v1/connections/{id}/kv/keys/{key}/value?cursor&count
```

> **实现口径（2026-11-05）**：上面两条带 `{key}` 的路径**没有按这个形状实现**——
> key 里可以含 `/`，路径参数表达不了（`a/b` 会被当成两级）。实际是 query 参数：
> `GET /kv/entry?key=` 取类型 + TTL + 元素数，`GET /kv/value?key=&cursor&count` 取值。
> 以 [`docs/12`](../12-api-contract.md) 为准，本节保留设计时的原样。

```
POST /api/v1/connections/{id}/kv/apply                 # 变更预案（同 Plan/Apply）
POST /api/v1/connections/{id}/kv/command               # 命令模式
```

路径里没有 `redis`：Mongo/ES 也会走同一族端点，只是内部映射不同。对关系型连接调用 `/kv/*` 返回 `400 CAPABILITY_UNSUPPORTED` 并说明「这是 key-value 端点」。

---

## 依赖选择

Redis 客户端选 `github.com/redis/go-redis/v9`（纯 Go、无 CGO、社区事实标准）。备选是自己实现 RESP2：协议本身不复杂，但 TLS、超时、连接池、`SCAN` 游标重试都要自己写，收益不明显而风险集中在自己不熟的协议细节上。ADR-0004 之后新增依赖只需过「纯 Go」这一条，它满足。

## 后果

### 必须做

- `driver.KVConnector` 与相关值类型（`KVScanOptions` / `KVPage` / `KVEntry` / `KVValue` / `KVMutation` / `KVResult`）。
- `service.KVExecutor`：风险分级 → 摩擦 → 审批 → 审计，与 SQL 路径同构。
- `policy` 的 Redis 命令风险表 + 「未知命令按 R4」。
- `api` 的 `/kv/*` 端点与 openapi 声明。
- 界面：keyspace 浏览器（`SCAN` 游标）、按类型的值编辑器、命令模式输入框（结构化：命令 + 参数，不做自由文本解析）。

### 明确不做（第一版）

- **集群模式**：只支持单机/主从。集群下多 key 命令需要 hash tag、`SCAN` 要逐节点，实现与测试成本远高于第一版价值。检测到集群时 `Health` 报告 `Warnings` 并在 UI 标注「集群未验证」，连接照常可用。
- **模块类型**（RedisJSON / RediSearch / RedisTimeSeries）：只做 `string` / `hash` / `list` / `set` / `zset` / `stream` 六种内建类型。
- **`KEYS`**：命令模式里命中会拒绝并提示改用 `SCAN`。
- **备份/恢复**：不在本 ADR 范围（`docs/08`）。

### 已知风险

- Redis 没有事务语义可比 SQL：`MULTI/EXEC` 只是命令排队，出错不回滚。所以变更预案**不承诺原子性**，必须在预案里写明「Redis 的多命令不保证回滚」，这与关系型引擎的 Plan/Apply 语义不同，不能套用同一句文案。
- 大 key 上 `MEMORY USAGE` 可能阻塞，需要限流与超时（`docs/04` §5.6 已提）。

---

## 备选方案与被否原因

| 方案 | 否掉的理由 |
|---|---|
| Redis 也实现 `Metadata`，把 key 映射成「表」、把 hash field 映射成「列」 | `string`/`list`/`zset` 没有列；`DDL`/`Dependents` 无处安放。会得到一堆永远返空的实现，让上层误判能力存在 |
| 在 `api` 里按引擎名分支 | 违反 ADR-0005；每加一个 NoSQL 引擎都要改 `api` |
| 只做「命令模式」，不做结构化值编辑器 | 命令模式要求用户记住每种类型的命令与参数，对「看一眼这条 hash 有什么」这种最高频需求是倒退 |
| 先做 Mongo 再做 Redis | Mongo 有 BSON 文档模型，与 `TableDetail` 的映射争议更大；Redis 的 key→值模型最简单，先做它能最快验证这套接口是否站得住 |
