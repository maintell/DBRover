# ADR-0015 · 连接器自报目标标识：恢复覆盖前「把库名写出来」

- 状态：**已采纳**（SQLite 覆盖路径有集成用例；MySQL 的拒绝分支用能力位伪装验证）
- 日期：2026-11-05
- 影响面：`internal/driver`（`Connector.Target`）、`internal/driver/{pg,mysql,sqlite}`、`internal/service/backup.go`（`RestoreRequest` / `Restore`）、`cmd/dbrover`（`restore -confirm-target`）、`docs/08 §3.1 / §7`
- 关系：兑现 `docs/08` 验收 3 里那句一直没做的「输入目标库名」

---

## 背景

`docs/08` 验收 3 的要求是「恢复到已有对象的库被拒绝」，并写明希望有「输入目标库名」这一层确认。
v1 只做了一个布尔开关 `-allow-existing`，档上写着「可显式覆盖」。查证这件事时发现两处问题：

1. **布尔开关挡不住「看错库」**：它不需要人回忆任何信息，手一滑加上就过去了。真正能拦住
   错误的是「让人把这个库的名字写出来」——写错时，人自己就会发现写的和连的对不上。
2. **`-allow-existing` 其实从来没工作过**：恢复执行的是容器里的 `CREATE TABLE`（不带
   `IF NOT EXISTS`），目标表已存在时必然失败。也就是说「可显式覆盖」这句是写着的、坏的——
   没有任何用例走过这条路（`AllowExisting=true` 一次都没被测试覆盖）。

## 决策

### 1. 连接器自报 `Target()`

```go
type Connector interface {
	Engine() model.Engine
	Capabilities() model.Capabilities
	// Target 是这个连接的目标标识：关系库是 catalog，SQLite 是文件路径。
	Target() string
}
```

为什么由连接器自报，而不是让调用方传：调用方传的是它**以为**的目标，正好挡不住「看错库」。
这里刻意不暴露 `ConnectConfig`——那个结构里带着口令，接口上一旦有它，早晚会被日志打出来。

### 2. 用 `ConfirmTarget` 取代 `AllowExisting`

目标非空时必须把目标名写出来，且与 `Target()` 一致（大小写不敏感）。三种失败各有话说：

- 没写：`目标 <名字> 已有同名对象 a、b；确认要覆盖，请把目标名原样写出来`
- 写错：`你写的目标名是「x」，而这条连接实际指向「y」—— 先确认有没有连错库`
- 连接器报不出名字：如实说「这条连接没有报告目标标识」，不拿空白引号糊过去

### 3. 覆盖 = 先删冲突对象，再按备份重建

删视图在前、删表在后（视图依赖表），全部在同一个事务里，失败整体回滚。报告新增
`Replaced` 字段点名被替换的对象——原来的数据确实没了，这件事必须出现在报告里。

### 4. 只在 DDL 能回滚的引擎上允许覆盖

MySQL 的 DDL 隐式提交：`DROP` 成功而 `CREATE` 失败，会留下「旧的删了、新的没建起来」的库。
这种事不该由一个命令行开关授权，所以 MySQL 上明确拒绝并说明「请先清空目标库」。

## 后果

**正面**

- 看错库会被拦住，而不是等到数据被覆盖之后才发现。
- 「可显式覆盖」从写着变成真的能用，并且报告会说清删了什么。
- 不可回滚的引擎上不会出现「删完才失败」的中间态。

**负面 / 必须说清**

- SQLite 的 `Target()` 是完整文件路径，确认时要写一长串。这是它的真实身份，不以“好看”为标准。
- `-allow-existing` 被移除：已有脚本要改成 `-confirm-target <目标名>`。项目在 0.x，不留双写兼容。
- MySQL 上仍然只能恢复到空库；想要覆盖得自己先清空。这条是能力边界，不是待办。

## 验证计划

1. `TestRestoreRequiresMatchingTargetName`（SQLite）：不写名字被拒并说明怎么写 → 写错被拒且
   错误里同时出现「你写的」与「实际指向」 → 写对后覆盖成功、`Replaced` 点名 3 个对象、每表仍 1 行
   （覆盖不是叠加）。
2. `TestRestoreRefusesOverwriteWhenDDLCannotRollBack`：把 SQLite 连接伪装成 `DDLTransactional=false`，
   断言拒绝并说明「不能回滚」。
3. 门禁会真红：把覆盖前的「删冲突对象」去掉，第 1 条第三步立刻红（表已存在）。

