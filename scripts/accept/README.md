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
| `bulk-engines.ps1 -Engine mysql -Port 13306 -User root` | 同上但 3000 行 3 批，重点是**逐批都是「改成相同值」**时 `RowsAffected` 仍等于本批主键数（MySQL 的 `clientFoundRows`） | MySQL 或 PostgreSQL |
| `precision.ps1 -Engine mysql -Port 13306 -User root` | `BIGINT` 上下界与 `NUMERIC(38,10)` 极值在「独立 SQL → 表浏览接口 → 原样写回 → 改成新精度值」全链路逐字符一致；反向验证 JSON 数字形态的大整数被 `400 NUMBER_PRECISION_LOSS` 拒绝 | MySQL 或 PostgreSQL |
| `compare-cross.ps1` | 跨引擎结构比对：MySQL ↔ PG 的同构表应当**零差异**（主键索引不误报、`BTREE`/`btree` 等价、整数显示宽度已归一） | MySQL + PostgreSQL |

## 约定

- 端点与端口写死在脚本里（7791–7796），跑之前先确认没被占用。
- 每个脚本自己 `Start-Process bin\dbrover.exe serve` 并在 `finally` 里停掉，不依赖外部服务。
- 失败一律 `throw`，不做「打印一行警告继续跑」——验收脚本的意义就是能红。
- 跑完记得删测试容器：`docker rm -f dbrover-my dbrover-pg`。
