# CHANGELOG

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)；版本号遵循语义化版本。

## [Unreleased]

### 新增

- **文档基线**：14 份设计文档 + 7 份 ADR（技术栈 / Agent 工具边界 / Plan-Apply 分离 / 环境模型 / 跨引擎差异归能力与方言 / 依赖检查归驱动 / 非关系型连接器模型）。
- **方言层**（`internal/driver/dialect`）：词法分析、语句切分、指纹归一化、语句分类、条件分析、标识符引用。覆盖 PostgreSQL / MySQL / SQL Server / Oracle / SQLite 五种方言，含 dollar-quote、`DELIMITER`、`GO`、Oracle `/`、嵌套块注释、`]]` 转义。
- **风险与摩擦**（`internal/policy`）：语句风险 `R0`–`R4`；打扰强度 `F0`–`F5`，由「环境 × 风险」矩阵决定，含变更集路径、脚本长度、影响行数、备份缺失等修正项。
- **连接器契约**（`internal/driver`）：`Connector` / `Session` / `Metadata` 接口、能力声明、一致性测试套件 `drivertest`。
- **SQLite 连接器**（`internal/driver/sqlite`）：元数据、查询游标、事务、只读双层强制（`mode=ro` + `PRAGMA query_only`）。
- **MySQL 连接器**（`internal/driver/mysql`）：`information_schema` 元数据、游标、事务；`clientFoundRows=true` 让「改成相同值」不被误判为并发冲突。
- **PostgreSQL 连接器**（`internal/driver/pg`）：`pg_catalog` 元数据、自行拼装 DDL、参数占位符词法级改写（`?` → `$n`，原生 `$1` 原样透传）。
- **执行门**（`internal/service`）：切分 → 分级 → 摩擦 → 审批 → 审计；整批原子拦截（任一条被拒则全不执行）。
- **结果游标与分页**：流式游标（5 分钟 TTL、独立清理协程、占用互斥）+ 键集分页（无主键表显式拒绝）。
- **数据变更集与变更预案**（`internal/service/change.go`）：行级 insert/update/delete 编译成参数化语句 + 自动生成回滚语句；乐观并发按「主键 + 被改列原值」定位；`RowsAffected` 与预期不符即整批回滚；无主键表拒绝编辑；预案只由服务端保管并按指纹与有效期校验。
- **界面行内编辑**（`internal/webui`）：有主键且连接可写的表可直接改单元格，脏单元格高亮 + 顶部「待提交 N 处修改」，提交先出确认弹窗（风险、打扰、将执行的语句、回滚语句），确认后才 `apply`。
- **表数据筛选与排序**（`service.TableRows` + 界面工具条）：`where` 条件 AND 组合、单列排序（主键恒为末位排序键以保证键集分页不漏行）、页大小 100–1000；列名与操作符走白名单，写错列名返回 `400 FILTER_INVALID`。改视图前若有未提交改动会先确认。
- **一键撤销（10 分钟）**：应用成功后服务端把预案里已算好的反向语句登记成一张 `undo-` 预案，撤销就是把它 POST 回同一个 `apply` 端点——同风险、同摩擦、同审批，不给撤销开后门；撤销不可再撤销；不可逆变更返回 `undo: null`。
- **`NULL` 与空串显式区分**：`NULL` 显示为 `NULL`、空串显示为 `''`；右键菜单「设为 NULL」/「设为空串」二选一；原值为 NULL 时定位条件用 `IS NULL`（用 `= ?` 绑 NULL 永远匹配不到行）。
- **关闭页面拦截**：有待提交改动时 `beforeunload` 拦一次。
- **结构设计器（v1）**（`internal/service/ddl.go` + `internal/webui`）：表结构详情与 DDL 原文、**建表**、加列、删列、**改列类型（带收窄预检）**、**索引建删（带唯一索引重复值预检）**、表单改动即时预览将要执行的 DDL；结构变更复用同一套 Plan/Apply，预案带**表定义哈希**作为前置条件（建表则要求执行时表仍不存在），被他人改过即 `409 SCHEMA_CHANGED`。建表必须给主键且主键列强制 `NOT NULL`；删列展示非空行数并标不可逆；改列类型前先数越界数据；建唯一索引前先查重复值，两者不满足都返回 409 并带样本行；DDL 事务性由 `Capabilities().DDLTransactional` 决定，MySQL 上明确提示「隐式提交，回滚只是补偿」。
- **跨引擎差异的写法约定**（ADR-0005）：能力有无进 `model.Capabilities`（本次新增 `AlterColumnType`，SQLite 为 false），语句文本归 `internal/driver/dialect`（本次新增 `AlterColumnTypeSQL`），`service` / `api` 不再判断引擎名。
- **连接器契约收紧**：`Describe*` 遇到不存在的对象必须返回 `driver.ErrObjectNotFound`，不得返回零列的空结构。SQLite / MySQL / PostgreSQL 三个驱动原先都会把「表不存在」说成「表没有列」，导致 API 对已删除的表返回 200 空结构、变更预案给出误导性提示。由 `drivertest` 的 `DescribeMissingObjectErrors` 用例锁住（含变异验证）。
- **条件批量修改**（`internal/service/bulk.go` + `POST /bulk/plan` + 界面「批量修改」）：按当前筛选一次改多行，不加载行。生成预案前用 `COUNT(*)` 精确统计影响行数并写进 `expect_rows`，执行时行数不符即整体回滚；空 `WHERE` 等于全表写，必须回填表名；影响 ≤1000 行逐行生成回滚（可一键撤销），超过则不生成回滚、标不可逆并要求显式接受；确认弹窗给最多 5 行前后对比。主键列禁止批量修改。
- **批量修改分批与中止**（`internal/service/bulk_exec.go` + `apply?stream=1`）：影响超过 1000 行且是单列主键时，按主键键集分批推进（`pk > 上批末值 ORDER BY pk LIMIT 1000`），每批一个独立事务；键集只用来挑行，改行仍是 `WHERE pk IN (...)`，否则会越过本批边界。`apply?stream=1` 用 NDJSON 逐批回报 `{"type":"progress","done","total"}`，最后一行是 `result` 或 `error`；界面显示「已提交 N / 总数」并可「停止」。中止在批与批之间生效，已提交的批不回滚、预案保留（`SET` 是常量，重新提交幂等），返回 `409 CHANGE_APPLY_ABORTED`。被审批拦下时还没有进度，仍是普通 JSON + 428，不会把状态码藏进流里。复合主键与无主键表退回单事务，并在预案 `note` 里写明。
- **结构比对（v1）**（`internal/service/compare.go` + `POST /compare` + 界面「比对」）：两个已打开连接逐表比对，输出 `only_left` / `only_right` / `changed` / `need_review` 与逐列原因（中立类型、原始类型、可空、默认值、注释）；跨引擎的类型差异一律交人工判断。升级脚本只生成增量（建缺失的表、加缺失的列），删除与改类型进 `need_review`。只读：不生成预案、不改任何一边。- **外键孤儿数据预检（只读）**（`internal/service/fk_precheck.go` + `POST /connections/{id}/fk/precheck`）：检出子表里父表找不到对应行的数据（任一子列为 NULL 不计入），判断父表参与列是否唯一（主键或**列集合完全一致**的唯一索引——(a) 的单列唯一索引不能证明 (a,b) 唯一），有孤儿即阻断并给最多 5 条样本。加外键的 DDL 本轮未做：SQLite 不支持 `ALTER TABLE ADD CONSTRAINT`，MySQL/PG 暂无可用真机验收，不把没验过的语句交给用户。
- **结构比对补上索引级差异**：按「列序列 + 唯一性 + 谓词」为键匹配、不按名字（名字不同不构成功能差异，只记进 `need_review` 说明）；主键索引不当作普通索引处理。索引的增删不丢数据，所以 `upgrade` 与 `downgrade` 两个方向都生成脚本——这是比对第一次能给出真正可用的回退语句。列的删除与改类型仍进 `need_review`。
- **结构比对先归一化类型文本**：`character varying(n)` 与 `varchar(n)`、`integer` 与 `int` 是同一类型的两种打印方式，`bigint(20)` 的显示宽度 MySQL 8 已废弃，归一后不再报差异；`decimal(p,s)` 的精度原样保留。不做「聪明的类型映射」，只抹平同义写法——此前 MySQL ↔ PG 的同构表每列都会稳定误报一条类型差异，现在同构表零差异。
- **加外键（`add_foreign_key`）**：结构设计器现在能加外键了，而不是只做只读预检。生成预案前先跑**同一套**孤儿数据与父表唯一性检查，不通过直接 409 `FK_PRECHECK_BLOCKED` 并带样本；动作子句走白名单（CASCADE / RESTRICT / NO ACTION / SET NULL），`SET NULL` 撞上 NOT NULL 引用列提前拒绝（MySQL 会当场拒绝，PG 会建成功、删父行时才报错，后者更难查）；外键名留空时按 `fk_<表>_<列>` 生成——回滚要靠这个名字摘约束，随机名等于让撤销无从下手。回滚是 `ALTER TABLE … DROP FOREIGN KEY/CONSTRAINT`，不动数据。SQLite 用 `Capabilities().AddForeignKey = false` 声明不支持（它的 ALTER TABLE 只认 RENAME / ADD COLUMN），接口明确拒绝而不是生成必然失败的语句。
- **连接级展示时区（`display_timezone`）**：时间值按连接配置的 IANA 时区渲染，空 = UTC；在驱动返回行时统一换算，所以表浏览、查询分页、导出、游标续页看到的是同一个时区。写回按列类型归一：带时区的列换算到 UTC（保住瞬间），不带时区的列保留墙上时钟（保住用户看到的 12:00），`DATE`/`TIME` 取字面值不跨零点。时区名拼错直接 400，不静默退回 UTC。详见 docs/06 §1.4。
- **修掉两处会让时间静默挪 hour 的缺陷**：① MySQL 的 `TIMESTAMP` 在 SELECT 时按**会话**时区换算，之前会话是服务器默认（可能 +08:00）而驱动按 `loc=UTC` 解析，取出来整体偏 8 小时——现在握手时 `time_zone='+00:00'`，PG 同理 `timezone=UTC`。② 接口读出去的是带偏移的 RFC3339，原样写回 MySQL 会报 `1292 Incorrect datetime value`——「读出来再存回去」这条最普通的编辑动作之前根本走不通。
- **PG 只读改到连接参数**：`default_transaction_read_only=on` 之前是开完池再 `SET` 一次，而 `sql.DB` 是连接池——那条 SET 只作用于恰好被选中的物理连接，池里其余连接仍然可写。现在写进 DSN，每条连接都带上。
- **删除前的依赖检查 + `drop_table` 动作**（ADR-0006）：`driver.Metadata` 新增 `Dependents`，每项带 `Exact`——入向外键三个引擎都精确（`key_column_usage` / `pg_constraint` / `PRAGMA foreign_key_list`），视图依赖在 PostgreSQL 上走 `view_table_usage`（精确）、在 MySQL / SQLite 上只能拿定义文本做词边界匹配（标「可能误报」）。删表被任一依赖引用即 `409 DEPENDENCY_BLOCKED` 并点名引用方；删列只按**精确**信息阻断（引用该列的入向外键），视图依赖写进预案 `notes` 让人自查——这条不对称是有意的，视图里可能只是 `SELECT *`，用猜的规则会把正常操作挡死。删表必然不可逆，回滚语句只是把空表建回来，预案里明说「数据不会回来」。
- **建索引的在线变更建议**（`Dialect.IndexBuildAdvice`）：建索引预案会带上「怎么不锁表」的引擎建议——PostgreSQL 提示 `CREATE INDEX CONCURRENTLY` 并**明确说明它不能在事务里跑、需要到 SQL 工作区单跑**（本工具的执行门是单事务的，直接生成它等于每次都失败）；MySQL 提示 `ALGORITHM=INPLACE, LOCK=NONE`；SQLite 不给。只给建议、不自动生成那条注定失败的语句。
- **结构比对补外键与检查约束**：外键按「本表列 → 引用表 + 引用列」为键、不按名字，动作子句（`ON DELETE` / `ON UPDATE`）参与比较但不参与键——同一个关系改了删除行为会报成「一条有差异」，而不是变成互不相干的一删一增。左侧引擎支持 `AddForeignKey` 时给出加/删约束脚本，不支持时（SQLite）进 `need_review` 说明要重建表，**不给必然失败的语句**；检查约束按归一化表达式匹配，因为还没有加约束的动作，一律进 `need_review`。三个集合的比对骨架抽成了泛型 `diffByKey`：三份几乎相同的循环最容易在改一处时漏掉另外两处。
- **非关系型引擎的接口模型（ADR-0007）**：Redis / Mongo / ES 不实现 `Connector`/`Metadata`——它们没有「表 / 列 / SQL」这套语义，硬套只会得到一堆永远返空的实现。改为一族独立接口 `KVConnector`（`Scan` / `Inspect` / `Value` / `Mutate` / `Command`），`api` 靠**类型断言**而不是引擎名选路径（ADR-0005 同一条原则）。写路径复用同一套执行门、审计与摩擦分级，但 Redis 命令风险表分开（不在表里的命令按 `R4` 处理）。`docs/04 §5.6` 同步补上了能力契约、失败降级与验收口径。
- **预执行检查：长事务与锁等待**（`Metadata.Blockers`）：MySQL 查 `innodb_trx` 与 `innodb_lock_waits`，PG 查 `pg_stat_activity` 的 `idle in transaction` 与 `wait_event_type='Lock'`，SQLite 恒为空（单写者模型，写冲突直接 `SQLITE_BUSY`）。**锁等待阻断**（`409 LOCK_WAIT_BLOCKED`），**长事务只写进预案 `notes`**（最多 3 条）——忙碌的库上永远有人开着事务，用它阻断等于关掉整个结构设计器。MDL 未做：MySQL 5.7 没有 `performance_schema.metadata_locks`，做不了就不假装能做。
- **复合主键表也能分批**：键集条件按列展开成 OR（`(a>?) OR (a=? AND b>?)`），不用行值比较——后者在 SQLite 3.15 之前不可用。代价是「按主键定位」那一半也要写成 `(a=? AND b=?) OR …`，每多一组多两层表达式，而 SQLite 的表达式树深度上限是 1000，所以复合主键的批大小降到 200（单列主键仍是 1000，用 `IN (...)` 只占一个节点）。先用 1000 跑过一次，SQLite 直接报 `Expression tree is too large`。
- **筛选支持 `IN` 与 `BETWEEN`**（docs/06 §4.1 一直列着但没做）：`FilterCond` 新增 `values` 数组，`IN` 任意个、`BETWEEN` 恰好两个（闭区间）；不复用 `value` 拼逗号——「值里本身带逗号」是真实数据（测试里就有一条 `a,b`），拼起来就没法还原成一个还是两个值。空 `IN` 与单值 `BETWEEN` 返回 `ErrInvalidFilter` 而不是悄悄退化成「不过滤」。界面筛选器同步支持，条件标签对数组也显示完整。
- **Redis 连接器第一版（只读）**：按 ADR-0007 落地 `driver.KVConnector` 的实现，引入 `github.com/redis/go-redis/v9`（纯 Go、无 CGO）。`api` 改用**类型断言**分流——`Opener` 返回 `any`，关系型得 `Connector`、key-value 得 `KVConnector`，`api` 里没有一处判断引擎名。端点是通用的 `/kv/keys`、`/kv/entry`、`/kv/value`、`/kv/command`，key 用查询参数传（Redis 的 key 二进制安全，斜杠与空格都合法，放进路径会被转义规则咬住）。**命令模式第一版只放行只读命令**：写命令一律 `403 POLICY_WRITE_NOT_ENABLED`（写路径要过审批与审计，尚未做），`KEYS`/`DEBUG` 直接拒绝并给替代方案。真机 Redis 8.0.5 上验过 `SCAN`、`TYPE`/`TTL`/`MEMORY USAGE`、读值与三类门禁。
- **修掉界面里 PostgreSQL 选项值写错**：`ENGINES` 里写的是 `pg`，而服务端只认 `postgres`，选中它必然得到「暂不支持的引擎」。验收脚本一直显式传 `-Engine postgres`，所以从没暴露。新增 `TestWebuiEngineNamesAreAccepted` 用 `model.AllEngines()` 反向校验，防止再脱节。
- **Redis 写路径**（`service.KVExecutor` + `POST /kv/apply`）：与 SQL 同构的一道门——风险分级 → 摩擦 → 审批 → 审计。审批凭证绑定**命令指纹**：拿 `SET k` 的凭证去改另一个 key、或去执行一次写操作，都返回 409 `POLICY_APPROVAL_MISMATCH`（`TestKVWriteGateAndApprovalBinding` 逐条断言）。只读连接上的写命令映射成 `POLICY_READ_ONLY`，与 SQL 路径同一条规则。结构化写操作（`set_string`/`set_field`/`delete_key`/`delete_item`/`push`/`expire`）先映射到一个真实命令名再分级，避免为它单独维护第二张风险表。**刻意不做「一次多条命令」的预案**：Redis 的 `MULTI/EXEC` 出错不回滚，套关系型那套「整批回滚」文案会骗人。
- **Redis 界面**：连接列表里 `kind=keyvalue` 的连接自动切成 keyspace 浏览器（pattern 输入 + `SCAN` 游标「更多…」），选中 key 显示类型 / TTL / 元素数 / 体积 / 编码，按类型渲染值（string、hash、zset、set、stream、list 各一种表格），并给出「改值 / 写字段 / 删 key」与命令模式输入框。写操作按钮在只读连接上不出现——不是点了再报错。`kind` 由服务端**类型断言**得出，界面不记「哪些引擎是 NoSQL」。浏览器实测：列出 6 个 key → 点 `user:1` 看到 `string / 永不过期 / 元素 7 / 64 B / embstr` → 改值后变 `value-1-updated` 且体积随之变化 → 命令模式 `TTL user:1` 返回 `-1`。
- **手写 DDL（单向，`action: "raw"`）**：结构页多了「手写 DDL」入口——写一条语句 → 校验 → 走同一个 Plan/Apply，分级、审批、审计、竞态保护全部复用。三条刻意的限制：**一次只能一条**（多条没法保证原子性：MySQL 的 DDL 隐式提交，第二条失败时第一条已生效而回滚语句无处生成）；**只接受 DDL**（DML 走变更集，那里有乐观并发与逐行回滚）；**不做语义预检也不生成回滚**（收窄/孤儿/依赖都要理解语句含义，而手写 DDL 我们不解析），这两点写进预案 `notes` 并标不可逆。**没做**「SQL 反解回表单」——那需要完整 DDL 解析器，成本远大于收益。
- **流式导出**（`internal/service/export.go` + `GET /export`）：CSV 与 NDJSON，单条 SELECT 走游标分批取、边取边写；复用 `where`/`order`，导出的就是当前视图且不受分页限制；不要求主键，视图与无主键表也能导。大整数以字符串输出、二进制 base64、NULL 在 NDJSON 里是真正的 `null`。响应头推迟到首次写出，开跑前失败返回结构化错误而不是空文件配 200。界面加「导出」入口与格式下拉（状态栏给出字节数与首行，便于确认内容）。
- **回环 HTTP 服务**（`internal/api`）：token 鉴权、Origin 校验、连接管理、`exec`、`query`、`results/{id}/page`、`tables/{name}/rows`、审计查询、OpenAPI 契约（`/api/v1/openapi.json`）。
- **审计落盘**：JSONL 追加写（0600）+ 内存热区，`serve -audit <path>` 开启。
- **SSH 隧道**（`internal/infra/tunnel`）：多跳、密码/私钥/agent 认证、主机密钥失败关闭 + 信任落盘、拨号与握手超时。
- **内嵌 Web 界面**（`internal/webui`）：免构建、随二进制分发；连接、对象树、SQL 执行、结果网格、审批弹窗、服务器身份常驻显示。**临时实现**，正式前端仍按 ADR-0001 使用 React 栈。
- **CLI**（`cmd/dbrover`）：`version` / `health` / `tables` / `exec` / `serve` / `tunnel`，退出码 0（成功）/ 1（失败）/ 2（被策略拒绝）。
- **CI**：`ci.yml`（PR 与主干只跑测试）、`release.yml`（仅 tag 触发，只做交叉编译与发布）。规则由 `internal/ci` 的测试强制。

- **AI Agent 运行时**（`internal/agent`）：Provider 抽象 + OpenAI 兼容客户端（DeepSeek / 通义 / Moonshot / vLLM / Ollama 走同一套协议，用 `base_url` + `model` 区分）、14 个工具、权限三模式 × 环境上限 × 连接访问模式、执行循环（步数 / 时长 / 循环检测 / 同工具失败上限）、审批门（预案只由服务端保管，Agent 无法自批）。只读判定在服务侧（`ReadOnlyQuery`），不靠调用方自觉。接 HTTP（`/agent/status`、`/agent/conversations`、`/messages`（含 `?stream=1` NDJSON）、`/approvals`、`/export`）与界面（步骤时间线、可展开 SQL、审批卡片、会话导出）。
- **Agent 上下文检索与压缩**：按问题里的词检索相关对象（中文额外取 2~3 字前缀，因为中文没有空格），对命中的对象再查一次入向依赖（视图与外键），最多注入 40 个对象名 / 5 张表列定义；历史超过字符预算时把最早的工具结果截断并注明「已压缩」。
- **Agent 评测集**（`internal/agent/eval`）：14 条录制回放用例（只读分析 6、失败自愈 1、安全边界 4、提示注入 3），**SQLite / MySQL 5.7 / PostgreSQL 17 三引擎真机全过**；`TestEvalSuite` 断言不安全行为率 = 0、平均工具调用数 ≤ 8。
- **按光标所在语句执行**：`exec` 接受 `cursor`（UTF-8 字节偏移），服务端挑出光标所在的那一条；选中一段则跑选中的那段。切分仍在服务端，前端只提供偏移。
- **取消正在执行的语句**（`POST /connections/{id}/cancel` + 工具栏「取消」）：SQLite interrupt、PostgreSQL 断开即停、**MySQL 用 `KILL QUERY`**（会话为此钉在单条物理连接上，只取消本地 ctx 不够——`SLEEP()` 不读 socket）。
- **执行计划**（`POST /connections/{id}/explain` + 「计划」按钮）：SQLite 用 `EXPLAIN QUERY PLAN`、PG/MySQL 用 `EXPLAIN`、MSSQL/Oracle 明确 400。新增 `AnalyzePlan` 把计划翻成偏差项（全表扫描 / 扫整个索引 / 额外排序 / 临时树 / 嵌套循环）并在界面上高亮。
- **结构比对补视图定义**：只比查询体（`ALGORITHM` / `DEFINER` / `SQL SECURITY` 是部署属性，不计入差异），取不到定义时不判「相同」；差异表带两侧原文 `left_ddl` / `right_ddl`。
- **Windows 凭据库 + `dbrover secret`**（`internal/infra/secret`）：syscall 调 advapi32 的 `CredWriteW` / `CredReadW` / `CredDeleteW`；`secret set|clear|status` 的口令只从 stdin 读（不接受命令行参数）；Agent 的 API Key 取用顺序是「环境变量 → 凭据库」。非 Windows 返回 `ErrUnsupported`，不退化成明文文件。
- **`serve -open`**：就绪（`/api/v1/health` 通）之后用系统默认浏览器打开带 token 的地址，各平台一条命令，不引依赖。
- **多语句失败保留已完成结果**：错误体新增 `error.results`，带「失败之前已完成的语句」结果（含失败那条的 `err`），状态码与错误码不变；界面失败时先画已完成的结果。
- **百万行翻页压测**：100 万行表按 200 行/页翻 60 页（1.2 万行），总 619 ms、单页均摊 10.3 ms、HeapAlloc 增长 1.7 MB；从 50 万行之后取一页 13.1 ms。
- **`scripts/check.ps1` 磁盘前置检查 + `-TmpDir`**：临时目录所在盘可用空间 < 2 GB 时直接拒绝并给改法——盘满时链接阶段报 `There is not enough space on the disk`，看起来像代码错误。
- **写门禁自检**（`internal/agent`）：门禁判定只在 `policy.Authorize` 一处收口，现在由测试从**真实注册表**核对——`TestEveryWriteToolIsGated` 遍历每个 `Write` 工具（只读连接、只读模式一律拒；写模式下 R2+ 必须人批；自主模式只在 local/test 生效），`TestWriteToolsMatchContract` 钉住 `change.apply` 是 R2（降成 R1 等于取消审批），`TestEveryWriteToolHasAGateCase` 要求每个写工具都有一条「预期被拒」的评测用例。同时把评测集里硬编码的 `isWriteTool("change.apply")` 改成跟随 `Tool.Write` 派生——此前新增一个写工具会整片绕过「不安全行为率 = 0」。
- **跨库引用预检（PostgreSQL）**：`库.模式.表` 三段名在**碰连接之前**就拦下，返回 `400 CROSS_DATABASE_UNSUPPORTED`，提示另开连接或改用 `postgres_fdw` / `dblink`（真机核对：PG 17 上 `SELECT * FROM other.public.t` 报 `cross-database references are not implemented`，而两段名 `public.t` 只报「关系不存在」，语法本身是通的）。判断放在方言层（`Dialect.CrossDatabase`，ADR-0005）：字符串与注释里的点不算、数字里的点不算、两段名放行、四段名整体报出来；MySQL 的 `库.表`、SQL Server 的 `库.模式.表`、Oracle 的 `库.模式.表` 都是正常写法，不拦。为此 `Outcome` 新增 `BlockCode` / `BlockHint`：策略类拦截仍是 403/428，语句本身跑不通的是 400，客户端不会对着一个永远不会过的请求反复重试。Agent 侧同步把拦截原因原样回给模型（`TestQueryToolSurfacesCrossDatabaseBlock`）——此前被拦下只会看到 `EMPTY_RESULT` + 「确认语句是否为查询」，方向完全错了。
- **触发器能力声明改成真值**：MySQL / PG 一直声明 `Triggers: true`，但 `Objects(KindTrigger)` 里没有分支，调用会返回空列表——用户会读成「这张表没有触发器」，而不是「这个工具还不会查」。两处改成 `false`，SQLite 保持 `true`（走 `sqlite_master`，已补 `TestListsTriggers` 实证）。`TestTriggerCapabilityIsHonest` 把三个引擎的预期值钉住，新增引擎不写进这张表就直接失败（变异验证：把 MySQL 翻回 true 立刻红）。`docs/04` §3.2 补一行「触发器列表」如实标注。
- **锁等待阻断补上真机验证**：`checkBlockers` 早就把 `lock_wait` 判为阻断，但只有 `splitBlockers` 的单测——没人验过「真的有人在等锁时预案会被拒」。`TestHighRiskDDLBlockedByLockWait` 在 MySQL 5.7 上开两个事务造真实等锁（一个持行锁、一个排队），断言 `BuildDDLPlan` 返回 `ErrLockWait`；变异验证：去掉阻断判断即红。docs/07 第 11 条随之从 ⚠️ 转 ✅。
- **迁移断点续跑**：每批完成后写检查点（行数、下一批起始键、**累计摘要**），重启从检查点继续；键值带类型标记（`{"k":"i","i":123}`），否则续跑会把字符串喂给数值键。摘要也随检查点恢复，所以续跑后的整表摘要与一次跑完的一致——`TestMigrateResume`（2500 行搬 1000 停 → 续跑 → 2500 行且摘要相等）、`TestMigrateResumeNeedsCheckpoint`（无检查点明确报错，不静默从头）。只有真正搬完才删检查点：演练停在半路时删掉等于把续跑凭据扔了（第一版就写错，被 TestMigrateResume 抓到）。**已知窗口**：检查点在 INSERT 之后写，死在两者之间会重放该批并撞主键冲突，此时明确报错让人对账，不做 upsert。
- **迁移数据搬运与校验（单表）**：`MigrateTable` 按主键键集分批搬（`(a,b) > (?,?)` 复合键也支持），每批一条多行 INSERT 并走**目标端执行门**（审批/审计/策略照旧）；搬完比对行数与整表摘要，`VerifyMigratedTable` 可随时独立复查。无主键表退化一次性搬运，报告里写清「不可中断续跑 / 未做内容校验」。测试：2500 行 5 批摘要一致、改目标一行必被 `ErrMigrateVerify` 发现、无主键表的两条限制被标注。**过程中发现一处重复实现**：搬运动作与独立复查各写了一份摘要比对，变异验证时改坏其中一处、测试仍全绿——已抽成唯一的 `compareMigrateDigests`，现在短路它会让用例红。**未做**：检查点落盘与断点续跑（验收 6 只到部分）、索引/外键/默认值、CLI 入口。
- **迁移校验的原语**：`canonicalValue` / `rowDigest` / `tableDigest`（`internal/service/migrate_hash.go`）——把一行的列值编码成**跨驱动一致**的文本再哈希：整数与同值字符串编码相同（驱动返回类型不统一）、NULL 用 `\x00NULL` 哨兵与空串区分（这正是迁移里最常见丢数据方式）、时间统一到 UTC、二进制转十六进制。整表摘要按顺序累加，因此调用方必须按主键排序（换顺序得到不同摘要——这是「顺序不同」不是「数据不同」）。测试：`TestCanonicalValue`、`TestTableDigest`（同样行同摘要、换序必须不同）。键集搬运与检查点仍未做。
- **迁移数据搬运的口径定下来了**（[ADR-0011](docs/adr/0011-migrate-data-movement.md)，实现待做）：① 续跑按**主键键集**而不是行号/偏移——并发写入下偏移分页会跳行或重复，键集不会；单列 `pk > ?`、复合列 `(a,b) > (?,?)`，无主键表退化为一次性并明确标注不可续跑。② **不做 upsert**：`ON CONFLICT` 与 `ON DUPLICATE KEY` 语义不同，统一会把「覆盖」和「跳过」混在一起；从头重跑遇主键冲突就报错并要求先清空目标表（不替用户删数据）。③ 校验用**行数 + 按主键排序后的全量哈希**（NULL 用固定哨兵，避免 `1` 与 `'1'` 被判成不同）；无主键表只做行数校验并在报告里出声。④ 内存：流式游标 + 每批 1000 行多行 INSERT，上限约为「批大小 × 行宽」。
- **迁移建表预案**：`POST /migrate/change` 把预检出的 `CREATE TABLE` 做成结构预案，执行仍走 `/changes/{id}/apply`——审批、审计、竞态保护全部复用，不为迁移另开执行路径。**刻意不接受客户端传来的 DDL**：语句按 `decisions` 由服务端重算并过类型白名单，否则客户端就能绕过预检直接执行任意语句。顺带修正一条规则：**同引擎且目标表还不存在时直接 ok**（源类型在目标端本来就合法），不再逼人逐列决定；同引擎但目标类型不同仍走 review/blocked。测试：`TestMigrateChangeEndpoint`（预检→预案→执行→目标库真有表，含 428 时取审批 token 重试）、`TestMigratePlanEndpoint` 三个场景（目标表缺失→ready；类型不一致→逐列 review；补 decisions→ready）。
- **迁移预检接上 HTTP**：`POST /api/v1/migrate/plan {source, target, table, target_table?, decisions?}` 读两端结构，返回 `{plan, ready, ddl?}`——`ready=false` 时 `ddl` 不返回但 `plan` 仍在（逐列原因照旧），界面据此逐列提问。只读，不建表不搬数据。测试：`TestMigratePlanEndpoint`（目标表不存在→ready=false 且 plan 非空；目标建好后→ready=true 且 DDL 含引用后的列名）。写测试时踩到 `createConn` 固定用同一个 `api.db`，两个「连接」其实是同一个库，造不出目标表缺失的场景——改用已有的 `createConnAt` 指定另一个文件。
- **迁移结构生成（单表）**：`BuildMigrationDDL(dstEngine, table, src, plan, decisions)` 把预检结论渲染成目标端 `CREATE TABLE`——ok 列直接用预检类型，**review/blocked 列必须在 `decisions` 里给出目标类型**，缺一个就返回 `ErrMigrateNeedsDecision` 并点名（含 `review`/`blocked` 标注）；列名走方言引用、类型串过 `typePattern` 白名单（带分号的决策会被拒），保留 `NOT NULL`。测试覆盖「缺决策要点名」「决策生效」「非法类型被拒」三条。**仍未做**：数据搬运、校验、索引/外键/默认值、CLI 入口。
- **迁移预检核心**：`service.PlanMigration` 按 docs/08 §4.3 逐列给结论——**收窄一律 blocked**（长度/精度/小数位，附具体数字）、语义漂移一律 **review**（TINYINT(1) 的布尔歧义、unsigned bigint 溢出、ENUM/SET、jsonb→MySQL 无 GIN、uuid→char(36)、Oracle DATE 含时分秒、Oracle NUMBER 无精度、SQLite 亲和性）、**没命中规则也 review**（不假装安全）。同引擎同类型直接 ok，MySQL DATETIME 映射到 timestamp 视为保真。目标独有的列会单独提示「迁移不会写它，确认默认值」。14 条表驱动用例 + 一条目标独有列用例。**未做**：结构生成、数据搬运、校验、Oracle 空串探测、按实际值判超长；也没有 CLI 入口。
- **备份加密**：`backup create -passphrase-stdin` / `restore -passphrase-stdin`，口令只从标准输入读（命令行参数会被进程列表看到，环境变量会被崩溃转储带走）。PBKDF2-HMAC-SHA256（210k 迭代、16 字节盐）派生密钥，AES-256-GCM 每段独立 nonce，可与压缩叠加（先压后加密）。**头部保持明文**——列出对象和派生密钥都需要它；加密只覆盖段。口令丢了没有后门。测试：加密+压缩往返（文件里搜不到明文 SQL）、缺口令、错口令（错误同时提到「也可能被改动」，因为 AEAD 分不清）。
- **备份容器支持压缩**：`dbrover backup create -compress` 对每段 gzip，标志记在二进制头 flags bit1；`inspect` 会显示，`restore` 按标志解压。**续跑沿用文件里已写下的 flags**——否则 `-resume` 时忘带 `-compress` 会把同一个文件写成前后两种格式，读端只能猜。读端对未知 flags 位与加密位都明确拒绝（不是静默忽略）。测试：`TestBackupCompressedRoundTrip`（压缩后体积确实更小、头里置位、恢复后计数一致）、`TestBackupResumeKeepsCompression`（中断后不带 `-compress` 续跑，flags 仍是压缩）。
- **恢复报告会比对计数**：容器头新增每张表的 `indexes` / `foreign_keys` 计数（备份时取 `DescribeTable`，取不到就记 0 并跳过比对）；恢复后逐表重新统计，不一致进 `Mismatches`，CLI 打印「警告：以下计数与备份时不一致（数据已写入，但请人工确认）」。老容器没有这两个字段时跳过，不会把「当时没记」误报成「恢复丢了」。测试：`TestRestoreReportsCountMismatch`（造一个「99 个索引」的容器头，恢复后必须报）、`TestBackupKeepsIndexes`（正常恢复时 `Mismatches` 为空且头部记下了索引数）。
- **修掉一个会让恢复静默丢索引的缺陷**：SQLite 的建表语句不含独立索引（`sqlite_master.sql` 只存建表），而导出脚本只写 DDL + INSERT，于是备份→恢复后表是「裸」的——**唯一索引丢失意味着唯一性不再被强制**。修法：方言新增 `DDLIncludesIndexes()`（只有 SQLite 为 false），导出时按需补 `CREATE INDEX`；约束背后的索引跳过，避免撞名。`model.Index` 新增 `Constraint` 标记（SQLite 按 `origin='u'`、PG 按 `pg_constraint.conindid` 判定）。**这条是真实数据库纠正的**：我一开始以为 PG 也要补，真机跑出 `relation "idx_exp_rt_s" already exists`——PG 驱动拼 DDL 时已经把 `pg_get_indexdef` 追加进去了，MySQL 的 `SHOW CREATE TABLE` 也自带。恢复报告里也补了索引个数。测试：`TestBackupKeepsIndexes`（导出含 `CREATE INDEX`、恢复后索引在、且往里插重复值必须被唯一约束拒绝）、`TestSQLExportRoundTripOnRealEngines` 增加索引断言（MySQL 5.7 / PG 17）。
- **恢复禁止落在源库**：容器头新增 `source_id`（JSON 字段，向后兼容、不升版本号）——sqlite 记绝对路径，网络引擎记 `host:port/库名`，由调用方提供。恢复时目标身份与之一致即拒绝并点名源库（`TestRestoreRefusesSource`，同时验证「换个身份就能恢复」，免得这道理查变成谁也恢复不了）。这条只能挡手滑：身份串不是认证，想绕过的人换个 `-db` 写法就行——写在文档里，不当安全边界用。
- **备份恢复 v1**：`dbrover restore -file x.dbr -db dst.db -yes`。三条硬约束在服务层执行——**同引擎**（跨引擎要类型映射，v1 直接拒绝）、**目标非空默认拒绝**（点名冲突对象，`-allow-existing` 才覆盖）、**单事务**（引擎支持 DDL 事务时，任一段失败整体回滚）。恢复后给报告：对象数与行数（行数是恢复到库里再 `COUNT(*)` 读出来的，不是猜的）。不加 `-yes` 直接拒绝——恢复是写操作，不该敲下回车就发生。测试：`TestRestoreRoundTrip`、`TestRestoreRefusesNonEmptyTarget`、`TestRestoreRejectsOtherEngine`；手工冒烟验过「未加 -yes 拒绝 / 正常恢复后 `COUNT(*)`=1 / 目标非空被拒并点名 t1」。**未做**：源库身份校验（容器头还没记源库）、索引与外键状态、跨引擎迁移。
- **备份续跑（表级）**：按 ADR-0009 实现——每写完一张表追加检查点到 `<file>.progress`（JSON Lines，写临时文件再原子改名），`backup create -resume` 时读它、把容器截断到最后一条偏移、跳过已完成的表。容器格式与读端**零改动**。无检查点时返回 `ErrBackupNoCheckpoint` 并说清「需整库重跑」，CLI 不会在此时给出「可以续跑」的自相矛盾提示。测试：`TestBackupResumeAfterFailure`（注入写入失败→续跑→断言三段、每表只出现一次、检查点已删）、`TestBackupResumeNeedsCheckpoint`。**行级续跑明确不做**：要 v2 分块表 + 块级校验，收益只是一张失败的大表少读一部分。
- **备份容器 v1 + `dbrover backup`**：按 docs/08 §2.3 落地容器（magic + version + flags + header JSON + 每段长度前缀），每段就是该表的 `sql` 脚本，同引擎可回放。`backup create` 写、`backup inspect` 只读头列对象（不连库、秒级）。**边界写清**：未加密、未压缩、无续跑、无恢复/迁移、对象条目不含行数——这些都在 docs/08 §7 的 v1 口径表里逐条对照。测试：`TestBackupInspect` 用**测试自己实现的解析**读容器（不复用产器的解析函数，否则「写错了但读得懂」也会通过），`TestInspectBackupRejectsBadInput` 覆盖非 DBR 文件与版本不匹配（错误信息要说出期望版本）。
- **导出多一种格式：`format=sql`**：单表输出「建表 + INSERT」的可回放脚本，头部注明同引擎回放。字面量按方言写（二进制 `X'..'` 与 `'\\x..'::bytea`、布尔 `1/0` 与 `TRUE/FALSE`、字符串单引号双写），**不做跨引擎类型映射**——它服务的是「把一张表搬到另一个同引擎库」。测试：`TestSQLExportRoundTrip`（SQLite 导出→删表→回放脚本→逐值比对，含 NULL、含单引号的字符串、二进制）与 `TestSQLLiteral`（三种方言的字面量写法）；变异验证：去掉单引号转义即红。docs/06 原本把「SQL INSERT」写在「不做」清单里，这次按 docs/08 的逻辑备份方向把它改成已实现，并把「只保证同引擎」的边界写进契约。
- **路线图与两处口径对齐实际**：`docs/13` 的 M1 一直写「SQL Server 未做（磁盘已够，尚未开工）」，实际卡的是镜像拉不下来（附 ADR-0008）；M3 还写着「字符预算压缩」「未做注释检索」，两项都已完成；`docs/05` 的验收汇总停在「4 项通过」，实际 11/12。**同时纠正一处自己标满的地方**：`docs/07` 第 11 条只验证了锁等待阻断，MDL 探测（`performance_schema.metadata_locks`，需 MySQL 8）并没有做，上一轮把它标成整条通过是不准确的，现在如实标注。
- **检索补上注释匹配**：`meta.search` 的契约一直写的是「名称 + 注释」，实现却只匹配名称（§4.2 里注释那一路是 ⛔）。现在 MySQL 查 `information_schema` 的 `TABLE_COMMENT` / `COLUMN_COMMENT`，PostgreSQL 查 `obj_description` / `col_description`，注释命中排在名称命中之后（别让长说明把真名搜索挤下去）；SQLite 没有注释概念，如实跳过。`drivertest` 新增 `SearchMatchesComments`（写一段唯一 token 当注释 → 按它搜必须找到 → 无关词不该命中），MySQL 5.7 / PG 17 真机通过，SQLite 跳过；变异验证：去掉表注释条件即红。按业务含义找表（「订单未支付」）从此有路可走。
- **长会话压缩按窗口 85% 触发**：配了 `DBROVER_AGENT_CONTEXT_TOKENS` 就按**估算 token** 压缩（规则：CJK 1 字≈1 token、其余 4 字符≈1 token，偏保守），没配则沿用字符预算（24000），不猜一个假窗口。顺带修掉一个真缺陷：压缩原来按**字节**截 200，中文堆得多时会切出半个字，那条消息变成非法 UTF-8 发给模型就是乱码——现在按 rune 截断（`TestCompactKeepsValidUTF8`）。docs/11 第 14 条随之转 ✅，并写明「估算是近似、没有内置 tokenizer」。
- **结构设计器验收台账回填**：docs/07 的 12 条验收原本只有第 3、4 条有状态，其余散落在 §8.1 的实现台账里。这次逐条核对**测试是否真的存在**再回填：8 条标通过并指名用例；第 2 条如实写 3/5（SQL Server / Oracle 未实现）；第 11 条标「实现有、缺真等锁的集成用例」；第 12 条标「只有单向，DDL 反解回表单未做」。同时补上预览端点的首个自动化用例：`POST /schema/plan?preview=1` 只算语句不保管预案（拿预览 id 去执行必须 404）。
- **序列定义也取得到，并且验过「能重建且参数不丢」**：PostgreSQL 没有取序列定义的内置函数，由 `pg_sequences` 拼回 `CREATE SEQUENCE … AS <类型> INCREMENT BY … MINVALUE … MAXVALUE … START WITH … CACHE … [CYCLE]`。`drivertest` 的 `SequenceListing` 先用非默认选项建序列，再断言定义里必须出现那些片段，然后删掉、用取到的定义回放、重新列出来——期望片段来自**建序列的那一方**，不是自己的输出（早先只比对「原定义 vs 回放后的定义」是自证：少拼一个子句两次都少，比较照样相等；变异验证抓到了这一点）。
- **触发器定义取得到**：`Metadata.DDL` 按对象类型分派——SQLite 从 `sqlite_master`（按 type 收窄，避免与同名对象混淆）、MySQL 用 `SHOW CREATE TRIGGER`（按列名取值，5.7 叫 `SQL Original Statement`，按下标会随版本坏掉）、PostgreSQL 用 `pg_get_triggerdef`（排除 `tgisinternal`）。`/tables/{name}/schema?kind=trigger` 只回定义，对象树里点触发器能看到 `CREATE TRIGGER` 原文。`drivertest` 的 `TriggerListing` 增加「定义必须含 TRIGGER 与对象名」的断言，三引擎真机通过。
- **SQL 工作区能建触发器了**：切分器原先只认 Oracle 的 `BEGIN…END` 块，SQLite 的 `CREATE TRIGGER … BEGIN <语句>; … END;` 会被体内的分号切成两段，建触发器的语句 100% 失败。现在识别 `CREATE [TEMP] TRIGGER`，体内的分号不再当分隔符，并用 `CASE … END` 嵌套计数避免把 CASE 的 END 当成体结束。`TestSplitTriggerBody` 覆盖四个情形（含变异验证）。对象树也随之列出触发器与序列（按能力声明，不可点开看数据）。
- **触发器与序列真的能列了**：上一轮把 MySQL/PG 的 `Triggers` 说清楚（改成 false），这一轮把它实现掉并翻回 true——MySQL 走 `information_schema.TRIGGERS`，PostgreSQL 走 `pg_trigger`（排除 `tgisinternal`，否则每个外键都会冒出两条内部触发器）；PostgreSQL 顺带接上序列（`pg_class.relkind='S'`）。`drivertest` 新增 `TriggerListing` / `SequenceListing` 两个用例，靠每个驱动自己的钩子建真对象再列出来——MySQL 5.7、PG 17、SQLite 三引擎真机全过。
- **序列能力也改成真值**：PostgreSQL 声明 `Sequences: true`，但 `Objects(KindSequence)` 没有分支，对象树永远空着；改成 `false`，由 `TestSequenceCapabilityIsHonest` 钉住（翻回 true 即红）。`docs/04` 补一段「对象类型覆盖」说明：视图三引擎都有、触发器只有 SQLite、序列与例程还没做。
- **执行计划的能力声明改成真值**：SQLite / MySQL / PG 三个连接器的 `Capabilities().Explain` 长期写着 `false`（功能其实早就接了），调用方只能靠「点一次看报不报错」来判断。现在三处都是 `true`，`/connections/{id}/health` 增加 `capabilities` 字段，界面按它决定摆不摆「计划」按钮；`POST /explain` 先看能力再进方言，说了不支持就返回 `400 EXPLAIN_UNSUPPORTED`。`TestExplainCapabilityMatchesDialect` 双向钉住「声明 = 方言支持」（改回 false 即红），`TestHealthExposesCapabilities` 守住接口。
- **删列警示改成红色**：`plan.irreversible` 的提示从琥珀色改为红色（`.card p.danger`），与红色「不可逆」徽标一致——删列丢的是数据，和「需要注意」不是一个量级。`TestIrreversibleWarningIsRed` 钉住类名与配色（改回琥珀色即红），`TestDropColumnIsIrreversibleOnRealEngines` 在 MySQL 5.7 与 PostgreSQL 17 上验证预案标不可逆、`affected_rows` 是非空行数、回滚语句只能把列加回来。

### 安全

- **大整数不再静默丢精度**：`BIGINT` 超过 2^53 时，界面保持字符串、服务端按列类型还原；客户端若把它写成 JSON 数字，直接 `400 NUMBER_PRECISION_LOSS` 并提示改用字符串，而不是把丢了低位的值写进库。此前界面 `coerce()` 用 `Number()` 转换整数列，编辑 `9223372036854775807` 会变成 `...5808`。
- 只读连接的三层一致判定：`AllowsWrite()`、执行门拦截、驱动只读模式。
- 原始 SQL 的结果集不再可编辑：只有「表浏览」的网格允许改单元格。此前结果集的列可能来自别的表，拼出来的 `UPDATE` 会打错表。
- `access_mode` 严格校验：拼错的值直接 400，不会被当成未知模式放行。
- 审批改为 HMAC 凭证：绑定语句指纹与 10 分钟有效期；换语句复用即 409。
- 含顶层 `OR`/`NOT` 的条件不再判为「主键命中」——`WHERE id=1 OR 1=1` 判 `R4`。
- 游标 ID 全局唯一，避免多连接间结果集串库。
- Origin 校验：只接受同源或不带 Origin 的非浏览器客户端。

- **审计链接式哈希**：每条事件带 `prev_hash` / `hash`（`SHA256(prev_hash || 规范化 JSON)`），重启时接着上一条算；`GET /api/v1/audit/verify` 校验整条链并指到出问题的行号。改一行、删一行、插一行都能发现。
- **审计本地锚点**（`serve -audit-anchor <path>`）：每写 1 条（首条立即封）与每 100 条，把「条数 + 链头」连同 HMAC-SHA256 写进独立文件，密钥存系统凭据库。整段重写成一条自洽的新链时，链校验过得去、锚点过不去。
- **口令不进审计**：`CREATE/ALTER USER … IDENTIFIED BY`、`SET PASSWORD`、`ALTER ROLE … PASSWORD`、连接串里的 `password=` 一律脱敏成 `'***'`，账号名与动作保留。
- **明文口令门禁**：带口令建连接后，连接列表、审计接口、审计文件三处原始字节都不得含该口令（`TestNoPlaintextPassword`，含变异验证）。
- **两处 `[hidden]` 被 CSS 覆盖**的修复不再复现：取消按钮与待提交条都显式写了 `#x[hidden]{display:none}`。### 已知限制

### 已知限制

- 已实现 SQLite / MySQL / PostgreSQL 三个关系引擎与 Redis（只读浏览 + 写路径 + 命令模式）；SQL Server、Oracle、MongoDB、Elasticsearch 仍未实现。
- 结构设计器已覆盖建表、加列、删列、改列类型（收窄预检）、索引建删（唯一索引查重）、加外键（孤儿预检）、删表（依赖阻断）、手写 DDL（单向）；结构比对覆盖表 / 列 / 索引 / 外键 / 检查约束 / 视图定义。**未做**：视图与例程设计器、检查约束的增删、手写 DDL 反解回表单（需完整 DDL 解析器）。
- 执行计划只覆盖 3/5 引擎（MSSQL 需 `SET SHOWPLAN_ALL`、Oracle 需 `EXPLAIN PLAN FOR`），且只做「偏差项提示」，不做代价估算。
- 审计不防「能同时改审计文件、锚点文件并读到本机凭据库密钥」的本地管理员；真正的外部锚点（把链头发到另一台机器）未做。
- 凭据库只有 Windows 实现；macOS（Keychain）与 Linux（Secret Service）未做。
- Agent 的注释检索与向量召回未做（名称匹配 + 结构邻近已做）；评测集是录制回放，不检验模型本身的智力。
- 正式 React 前端未开始；`internal/webui` 是免构建的临时界面。桌面壳（Wails）未做，`serve -open` 是轻量替代。
- OpenAPI 契约已落文件并由测试约束，但尚未接入代码生成。
[Unreleased]: https://github.com/maintell/DBRover/commits/main
