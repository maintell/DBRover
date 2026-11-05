# 真实实例验收脚本

这些脚本不是单元测试：它们会启动真实的 `dbrover serve`，通过 HTTP 接口操作真实的数据库引擎，然后断言数据与响应。单元测试保证逻辑，这里保证「在真的引擎上也是这个结果」。

**这些脚本会写数据**：建自己的库/表，跑完删掉自己建的东西。不要指向生产库。

## 准备

```powershell
# 1. 构建（版本串无所谓，脚本只用二进制）
$env:CGO_ENABLED='0'
go build -trimpath -ldflags "-s -w" -o bin\dbrover.exe ./cmd/dbrover

# 2. 需要真机的脚本：在 debug-ubuntu 上起一次性容器（用后 docker rm -f）
docker run -d --name dbrover-my  -e MYSQL_ROOT_PASSWORD=testpass -e MYSQL_DATABASE=acc -p 13306:3306 mysql:5.7.38 --character-set-server=utf8mb4
docker run -d --name dbrover-pg  -e POSTGRES_PASSWORD=testpass     -e POSTGRES_DB=acc     -p 15432:5432 postgres:17
```

## 脚本

| 脚本 | 验什么 | 需要真机 |
|---|---|---|
| `bulk-20k-sqlite.ps1` | 分批批量修改：2 万行 20 批、NDJSON 进度逐条到达、`curl --max-time` 中途断开只保留已提交的批、预案仍有效可重新提交补齐 | 不需要 |
| `bulk-engines.ps1 -Engine mysql -Port 13306 -User root` | 同上但 3000 行 3 批，重点是**逐批都是「改成相同值」**时 `RowsAffected` 仍等于本批主键数（MySQL 的 `clientFoundRows`）；另含复合主键表 1500 行（批大小自动降到 200） | MySQL 或 PostgreSQL |
| `precision.ps1 -Engine mysql -Port 13306 -User root` | `BIGINT` 上下界与 `NUMERIC(38,10)` 极值在「独立 SQL → 表浏览接口 → 原样写回 → 改成新精度值」全链路逐字符一致；反向验证 JSON 数字形态的大整数被 `400 NUMBER_PRECISION_LOSS` 拒绝 | MySQL 或 PostgreSQL |
| `compare-cross.ps1` | 结构比对三段：① 跨引擎同构表**零差异**（主键索引不误报、`BTREE`/`btree` 等价、整数显示宽度已归一）；② 跨引擎单侧视图分类正确、原文带出、跨引擎的 `CREATE VIEW` 不自动进升级脚本；③ 同引擎（MySQL 两个库）同构视图**零误报**——`DEFINER` / `ALGORITHM` 头部不计入差异 | MySQL + PostgreSQL |
| `fk-add.ps1 -Engine mysql -Port 13306 -User root` | 加外键的端到端：孤儿数据 → 409 `FK_PRECHECK_BLOCKED` 带样本；清数据后出预案（含生成的外键名与动作子句）→ 执行 → 结构里出现外键 → 用返回的 `undo` 预案摘掉 | MySQL 或 PostgreSQL |
| `timezone.ps1 -Engine mysql -Port 13306 -User root` | 展示时区：同一条数据在 `display_timezone=Asia/Shanghai` 与默认（UTC）两条连接上读出的 RFC3339 是不是同一瞬间；再把值改成另一种带偏移的写法写回，用 UTC 连接读回仍同一瞬间（存的是瞬间，展示才换算） | MySQL 或 PostgreSQL |
| `deps-drop.ps1 -Engine mysql -Port 13306 -User root` | 删除前依赖检查：造「父表 + 引用它的视图 + 引用它的外键」→ 删表返回 409 `DEPENDENCY_BLOCKED` 并同时点名视图与外键 → 拆掉两者后出预案（不可逆 + 补偿建表 + 「数据不会回来」的说明）→ 执行后表真的没了 | MySQL 或 PostgreSQL |

## 约定

- 端点与端口写死在脚本里（7791–7796），跑之前先确认没被占用。
- 每个脚本自己 `Start-Process bin\dbrover.exe serve` 并在 `finally` 里停掉，不依赖外部服务。
- 失败一律 `throw`，不做「打印一行警告继续跑」——验收脚本的意义就是能红。
- 跑完记得删测试容器：`docker rm -f dbrover-my dbrover-pg`
- 这一轮的实际耗时（bdbdc1a，本机）：`bulk-20k-sqlite` 约 10s，其余每条 1–6s；12 次全跑不到 1 分钟。慢的是容器启动，不是脚本。
- **73b8714 的竞态检查**：`CGO_ENABLED=1 go test -race ./...`（PG 17 + MySQL 5.7 真机）全绿，`internal/service` 358s。新增的并发状态（慢查询采样器 / QPS 采样器 / 告警状态 / 指标留存互斥）在真实并发路径下没有报告数据竞争。注意 `-race` **必须** `CGO_ENABLED=1`（测试专用；发布构建仍是 `CGO_ENABLED=0`）。
- **最近一次全量回归（bdbdc1a）**：`go test ./...`（PG 17 + MySQL 5.7 真机）全绿 + 这 12 次验收全绿。踩过一个坑：起 MySQL 容器时漏了 `MYSQL_DATABASE=acc`，5 个脚本以 `DB_CONNECT_NO_DATABASE` 报错——**那不是回归，是环境少建了库**；起容器命令见本文上方。
- `go test ./...` 那一条慢得多（约 7 分钟，`internal/service` 与 `internal/agent/eval` 各占几分钟），它跑的是真机用例不是脚本，两者互补：脚本验「端到端能不能用」，用例验「每个边界对不对」。。

## 全量真机套件（不是这些脚本，是 `go test`）

`scripts/check.ps1` 只跑不依赖真机的那部分。要连真机跑**所有**用例（连接器一致性套件、迁移跨引擎、结构变更真机用例…）：

```powershell
$env:DBRover_TEST_MYSQL='root:testpass@192.168.2.88:13306'
$env:DBRover_TEST_PG='postgres:testpass@192.168.2.88:15432'
go test ./... -count=1 -timeout 1800s
```

**空实例上应当直接可跑**：真机用例会自己建 `dbrover_test`（`openReal` 与 `rowsTargets` 都做了）。这条是 2026-11-05 修的——此前必须先手工 `CREATE DATABASE`，否则整包跑在第一条真机用例上就红，而单独跑某一条又可能是绿的，极难排查。

排错顺序：先看是不是「库不存在」（我们的事），再看是不是容器没起来（环境的事），最后看**字符集**。

**MySQL 测试实例必须是 utf8mb4**。套件里有用例刻意写中文与 emoji（`hello 世界 🚀`）来验类型保真，实例是 `latin1` 时会报 `Error 1366: Incorrect string value: '\xE4\xB8\x96...'`——读起来像编码 bug，其实是容器少带了参数。起容器时带上：

```bash
docker run -d --name dbrover-my -e MYSQL_ROOT_PASSWORD=testpass -e MYSQL_DATABASE=acc \
  -p 13306:3306 mysql:5.7.38 \
  --character-set-server=utf8mb4 --collation-server=utf8mb4_unicode_ci
```

核对一句：`SELECT @@character_set_server` 必须是 `utf8mb4`。

## 引擎名

`-Engine` 接受 `mysql` / `postgres`；写 `pg` 会被脚本规范化成 `postgres`（服务端只认规范值，早先直接用 `pg` 会得到 `DB_CONNECT_FAILED: 暂不支持的引擎`）。这条是实际跑脚本时踩出来的，别改成只认 `pg`。

## 最近一次全跑（2026-11-05，HEAD bebe831）

这一轮在**一堆改动之后**再跑一遍：对象全局搜索端点、行级删除与复制、恢复覆盖前的目标名确认、以及六条文档一致性门禁（最后这一组不碰运行路径）。同一轮还跑了 **`go test ./...` 全量真机套件**（两个引擎都在场，12 个包全绿，含 `internal/agent/eval` 的 14 条录制回放与 `internal/service` 的迁移/比对/备份恢复用例）。7 个脚本 12 次运行（`bulk-engines` / `precision` / `timezone` / `fk-add` / `deps-drop` 各跑 MySQL 与 PostgreSQL 两遍，`compare-cross` 与 `bulk-20k-sqlite` 各一次）逐条退出码 0。

| 脚本 | 引擎 | 结果 |
|---|---|---|
| `bulk-20k-sqlite.ps1` | SQLite | 通过：20000 行 / 20 条进度 / 断线后重提补齐（幂等） |
| `bulk-engines.ps1` | MySQL 5.7 / PostgreSQL 17 | 通过：3000 行 3 批 + 复合主键 1500 行 8 批×200 |
| `precision.ps1` | MySQL 5.7 / PostgreSQL 17 | 通过：BIGINT 上下界与 NUMERIC(38,10) 极值逐字符一致；JSON 数字形态被 400 拒绝 |
| `timezone.ps1` | MySQL 5.7 / PostgreSQL 17 | 通过：两种展示指向同一瞬间，写回按 UTC 存 |
| `fk-add.ps1` | MySQL 5.7 / PostgreSQL 17 | 通过：孤儿阻断带样本 → 预案 → 执行 → 撤销 |
| `deps-drop.ps1` | MySQL 5.7 / PostgreSQL 17 | 通过：视图 + 外键双阻断并点名 → 拆掉后删表成功 |
| `compare-cross.ps1` | MySQL 5.7 ↔ PostgreSQL 17 | 通过：同构表零差异、单侧视图分类、同引擎视图零误报 |
