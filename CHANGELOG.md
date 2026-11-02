# CHANGELOG

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)；版本号遵循语义化版本。

## [Unreleased]

### 新增

- **文档基线**：14 份设计文档 + 5 份 ADR（技术栈、Agent 能力面、Plan/Apply 分离、环境模型、以及它们的取舍理由）。
- **方言层**（`internal/driver/dialect`）：词法分析、语句切分、指纹归一化、语句分类、条件分析、标识符引用。覆盖 PostgreSQL / MySQL / SQL Server / Oracle / SQLite 五种方言，含 dollar-quote、`DELIMITER`、`GO`、Oracle `/`、嵌套块注释、`]]` 转义。
- **风险与摩擦**（`internal/policy`）：语句风险 `R0`–`R4`；打扰强度 `F0`–`F5`，由「环境 × 风险」矩阵决定，含变更集路径、脚本长度、影响行数、备份缺失等修正项。
- **连接器契约**（`internal/driver`）：`Connector` / `Session` / `Metadata` 接口、能力声明、一致性测试套件 `drivertest`。
- **SQLite 连接器**（`internal/driver/sqlite`）：元数据、查询游标、事务、只读双层强制（`mode=ro` + `PRAGMA query_only`）。
- **执行门**（`internal/service`）：切分 → 分级 → 摩擦 → 审批 → 审计；整批原子拦截（任一条被拒则全不执行）。
- **结果游标与分页**：流式游标（5 分钟 TTL、独立清理协程、占用互斥）+ 键集分页（无主键表显式拒绝）。
- **回环 HTTP 服务**（`internal/api`）：token 鉴权、Origin 校验、连接管理、`exec`、`query`、`results/{id}/page`、`tables/{name}/rows`、审计查询、OpenAPI 契约（`/api/v1/openapi.json`）。
- **审计落盘**：JSONL 追加写（0600）+ 内存热区，`serve -audit <path>` 开启。
- **SSH 隧道**（`internal/infra/tunnel`）：多跳、密码/私钥/agent 认证、主机密钥失败关闭 + 信任落盘、拨号与握手超时。
- **内嵌 Web 界面**（`internal/webui`）：免构建、随二进制分发；连接、对象树、SQL 执行、结果网格、审批弹窗、服务器身份常驻显示。**临时实现**，正式前端仍按 ADR-0001 使用 React 栈。
- **CLI**（`cmd/dbrover`）：`version` / `health` / `tables` / `exec` / `serve` / `tunnel`，退出码 0（成功）/ 1（失败）/ 2（被策略拒绝）。
- **CI**：`ci.yml`（PR 与主干只跑测试）、`release.yml`（仅 tag 触发，只做交叉编译与发布）。规则由 `internal/ci` 的测试强制。

### 安全

- 只读连接的三层一致判定：`AllowsWrite()`、执行门拦截、驱动只读模式。
- `access_mode` 严格校验：拼错的值直接 400，不会被当成未知模式放行。
- 审批改为 HMAC 凭证：绑定语句指纹与 10 分钟有效期；换语句复用即 409。
- 含顶层 `OR`/`NOT` 的条件不再判为「主键命中」——`WHERE id=1 OR 1=1` 判 `R4`。
- 游标 ID 全局唯一，避免多连接间结果集串库。
- Origin 校验：只接受同源或不带 Origin 的非浏览器客户端。

### 已知限制

- 仅实现 SQLite 一个引擎；PostgreSQL / MySQL / SQL Server / Oracle / 非关系型引擎待做。
- 审计只做到「记录过什么」，链式哈希与完整性校验未实现，无法证明「没被人改过」。
- 正式 React 前端未开始；`internal/webui` 是免构建的临时界面。
- OpenAPI 契约已落文件并由测试约束，但尚未接入代码生成。
- 图形化界面未做：ER 图、执行计划可视化、结构设计器均未实现。

[Unreleased]: https://github.com/maintell/DBRover/commits/main
