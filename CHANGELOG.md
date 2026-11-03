# CHANGELOG

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)；版本号遵循语义化版本。

## [Unreleased]

### 新增

- **文档基线**：14 份设计文档 + 5 份 ADR（技术栈、Agent 能力面、Plan/Apply 分离、环境模型、以及它们的取舍理由）。
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
- **流式导出**（`internal/service/export.go` + `GET /export`）：CSV 与 NDJSON，单条 SELECT 走游标分批取、边取边写；复用 `where`/`order`，导出的就是当前视图且不受分页限制；不要求主键，视图与无主键表也能导。大整数以字符串输出、二进制 base64、NULL 在 NDJSON 里是真正的 `null`。响应头推迟到首次写出，开跑前失败返回结构化错误而不是空文件配 200。界面加「导出」入口与格式下拉（状态栏给出字节数与首行，便于确认内容）。
- **回环 HTTP 服务**（`internal/api`）：token 鉴权、Origin 校验、连接管理、`exec`、`query`、`results/{id}/page`、`tables/{name}/rows`、审计查询、OpenAPI 契约（`/api/v1/openapi.json`）。
- **审计落盘**：JSONL 追加写（0600）+ 内存热区，`serve -audit <path>` 开启。
- **SSH 隧道**（`internal/infra/tunnel`）：多跳、密码/私钥/agent 认证、主机密钥失败关闭 + 信任落盘、拨号与握手超时。
- **内嵌 Web 界面**（`internal/webui`）：免构建、随二进制分发；连接、对象树、SQL 执行、结果网格、审批弹窗、服务器身份常驻显示。**临时实现**，正式前端仍按 ADR-0001 使用 React 栈。
- **CLI**（`cmd/dbrover`）：`version` / `health` / `tables` / `exec` / `serve` / `tunnel`，退出码 0（成功）/ 1（失败）/ 2（被策略拒绝）。
- **CI**：`ci.yml`（PR 与主干只跑测试）、`release.yml`（仅 tag 触发，只做交叉编译与发布）。规则由 `internal/ci` 的测试强制。

### 安全

- **大整数不再静默丢精度**：`BIGINT` 超过 2^53 时，界面保持字符串、服务端按列类型还原；客户端若把它写成 JSON 数字，直接 `400 NUMBER_PRECISION_LOSS` 并提示改用字符串，而不是把丢了低位的值写进库。此前界面 `coerce()` 用 `Number()` 转换整数列，编辑 `9223372036854775807` 会变成 `...5808`。
- 只读连接的三层一致判定：`AllowsWrite()`、执行门拦截、驱动只读模式。
- 原始 SQL 的结果集不再可编辑：只有「表浏览」的网格允许改单元格。此前结果集的列可能来自别的表，拼出来的 `UPDATE` 会打错表。
- `access_mode` 严格校验：拼错的值直接 400，不会被当成未知模式放行。
- 审批改为 HMAC 凭证：绑定语句指纹与 10 分钟有效期；换语句复用即 409。
- 含顶层 `OR`/`NOT` 的条件不再判为「主键命中」——`WHERE id=1 OR 1=1` 判 `R4`。
- 游标 ID 全局唯一，避免多连接间结果集串库。
- Origin 校验：只接受同源或不带 Origin 的非浏览器客户端。

### 已知限制

- 已实现 SQLite / MySQL / PostgreSQL 三个关系引擎；SQL Server、Oracle 与非关系型（Redis / MongoDB / Elasticsearch）仍是占位目录。
- 编辑入口只覆盖最基础形态：JSON / BLOB / 复数类型走统一文本单元格，批次阈值分档、批量修改、时区显示策略均未做（见 `docs/06-data-editor.md` §8.2）。导出只支持 CSV 与 NDJSON（100 万行内存增长 10.6 MB，已压测），xlsx / SQL INSERT / Markdown 与测试数据生成未做。
- 结构设计器只做建表、加列、删列、改列类型、索引建删、表与列级结构比对：改默认值/可空、加外键与检查约束（预检已做）、视图与例程设计器、索引级比对、回退脚本、在线变更建议、依赖检查均未做（见 `docs/07-schema-designer.md` §8.2）。
- 审计只做到「记录过什么」，链式哈希与完整性校验未实现，无法证明「没被人改过」。
- 正式 React 前端未开始；`internal/webui` 是免构建的临时界面。
- OpenAPI 契约已落文件并由测试约束，但尚未接入代码生成。
- 图形化界面未做：ER 图、执行计划可视化、结构设计器均未实现。

[Unreleased]: https://github.com/maintell/DBRover/commits/main
