# ADR-0018 · 全量重置：只保留文档，实现从零重建

- 状态：**已采纳**
- 日期：2026-11-20
- 影响面：进程模型、跨模块接口、技术栈落地方式、安全边界、CI/发版流程、目录结构
- 关联：[ADR-0001](0001-tech-stack.md)、[ADR-0017](0017-desktop-shell-and-settings.md)、[../02-architecture.md](../02-architecture.md)、[../13-roadmap.md](../13-roadmap.md)

---

## 背景

DBRover 的实现由人与 Agent 混合编写，期间迭代替换过多次方向（临时 `internal/webui`、桌面壳、Agent 运行时、监控与备份/迁移分片），每一轮都选择「在既有代码上补齐」而不是回到契约重写。结果是：

- 同一职责存在多条路径（临时界面与正式界面目标、CLI 与 desktop 各自装配、executor 与 plan/apply 两套执行入口），读代码无法判断哪条是权威路径；
- 分层开始反向（`service` 里出现引擎特判、`driver` 里出现业务决策），与 AGENTS.md 的分层铁律冲突；
- 文档与实现长期不同步，`docs/13 §1.5` 的进度描述与实际行为已经不能互证；
- 新协作者（人或 Agent）每次都要先花大量成本辨认「哪些是历史包袱」。

判断：继续在现有实现上修补，成本高于按文档重建。文档本身是这套项目的既有资产——14 份设计文档、17 份 ADR、25 份产品设计文档已经把能力契约、验收口径、失败与降级写清楚，足以作为重建输入。

## 决策

### 1. 只保留文档，移除全部实现

移除（共 331 个受控文件）：

| 路径 | 内容 |
|---|---|
| `cmd/` | `dbrover` CLI/serve 入口、`dbrover-desktop` 桌面壳（独立 module） |
| `internal/` | `api` / `service` / `driver` / `policy` / `agent` / `host` / `infra` / `model` / `webui` / `ci` |
| `scripts/` | `check.ps1` 与 `accept/*` 验收脚本 |
| `.github/` | `ci.yml`、`release.yml` |
| `go.mod`、`go.sum` | 根 module 与依赖锁定 |

保留：

| 路径 | 内容 |
|---|---|
| `docs/` | 14 份设计文档、18 份 ADR（含本份）、`superpowers/specs` 设计说明 |
| `product-design/` | 25 份产品设计文档（IA、页面规格、交互、验收、实现计划） |
| `README.md`、`AGENTS.md`、`CHANGELOG.md` | 对外定位、协作约定、变更台账 |
| `.gitignore`、`.gitattributes` | 仓库元数据 |

### 2. 基线用标签固定，不靠「记得以前怎么写」

重置前的完整实现保留在标签 `reset-baseline-0.2.0`（提交 `29adedb`）。取回方式：

```
git switch -c pre-reset reset-baseline-0.2.0      # 整库回到重置前
git show reset-baseline-0.2.0:internal/service/x.go   # 只摘单个文件作参考
```

**否决**：直接删掉历史另起仓库。历史里至少有三类不能重造的结论——真机卡点（ADR-0008 的 blob 限速证据）、已证伪的抽象（ADR-0007 的 `KVConnector`）、踩过的引擎差异（`performance_schema.data_lock_waits` 这类只在真机暴露的行为）。丢掉历史等于把这些结论重新花一遍时间。

### 3. 重建顺序仍受 AGENTS.md 约束

文档先行不变：`docs/*.md` 契约 → ADR（涉及取舍时）→ 代码 → 测试 → 文档回填。重建不是「先写代码再补文档」，也不是推翻既有契约重开一轮设计——契约已经被前一轮验证过大半，重建的是实现质量与分层纪律。

## 影响

- **CI 归零**：`ci.yml` / `release.yml` 一并移除，`push main` 不再触发任何流水线；重建到可构建时须同时恢复「PR 只跑测试、发版只由 tag 触发、一次发版一条 runner」的约束（见 AGENTS.md §6）。
- **本地检查失效**：`scripts/check.ps1` 不存在，重建前 `go test ./...` 无意义。
- **文档一致性门禁消失**：`internal/ci` 原本校验 ADR 编号、`docs/NN §X.Y` 引用、CLI 开关与 README 参数表。该包被移除后，这类漂移只能靠人守；重建时必须把同等校验作为第一批交付物，否则文档会再次失真。
- **`docs/13 §1.5` 的进度表**描述的是重置前基线，已加注说明；重建进度另起记录，不回填旧表。
- **未跟踪本地产物**（`.tmp/`、`bin/`）不属于受控文件，本次不动；它们只是重置前的运行残留，重建后可另行清理。

## 验收

- `git ls-files` 只剩 `docs/`、`product-design/` 与根级文档/元数据；
- 标签 `reset-baseline-0.2.0` 存在且指向 `29adedb`，`git show reset-baseline-0.2.0:go.mod` 可取回旧 module；
- 文档内不再声称「由 `internal/ci` 守住」这类已失效的机制。
