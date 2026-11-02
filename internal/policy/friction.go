package policy

import (
	"encoding/json"
	"fmt"

	"github.com/maintell/DBRover/internal/model"
)

// Friction 是执行前的打扰强度。
type Friction int

const (
	F0 Friction = iota // 静默执行
	F1                 // 内联预览，点「提交」即确认
	F2                 // 弹窗：SQL + 影响行数 + 回滚
	F3                 // 输入对象名确认
	F4                 // 输入 + 冷却
	F5                 // 阻断
)

func (f Friction) String() string { return fmt.Sprintf("F%d", int(f)) }

// MarshalJSON 让摩擦等级在 JSON 中输出为 "F2" 而不是 2。
func (f Friction) MarshalJSON() ([]byte, error) { return json.Marshal(f.String()) }

// RequiresApproval 报告是否需要「额外的」人工确认（弹窗及以上）。
//
// F1 是内联预览：界面把将要执行的语句摊开，点提交即确认，不算打断。
// F2 起才需要调用方拿到审批凭证。这个边界与 service 执行门一致。
func (f Friction) RequiresApproval() bool { return f >= F2 }

// Blocked 报告是否被策略直接阻断。
func (f Friction) Blocked() bool { return f == F5 }

// Modifiers 是影响摩擦的上下文条件。
type Modifiers struct {
	// ChangeSetPath 表示经由变更集提交（路径自带语句预览）。
	ChangeSetPath bool
	// StatementCount 是本次脚本的语句数。
	StatementCount int
	// RowsAffected 是影响行数估计，-1 表示未知。
	RowsAffected int64
	// NoRecentBackup 表示目标表近 7 天无备份记录。
	NoRecentBackup bool
	// ReadOnlyConnection 表示连接为只读访问模式。
	ReadOnlyConnection bool
	// ReadOnlyRole 表示当前角色无写权限。
	ReadOnlyRole bool
}

// baseFriction 是「环境 × 风险」基准矩阵。
var baseFriction = map[model.Environment][5]Friction{
	model.EnvLocal:      {F0, F0, F0, F1, F2},
	model.EnvTest:       {F0, F0, F1, F2, F3},
	model.EnvStaging:    {F0, F1, F2, F3, F3},
	model.EnvProduction: {F0, F1, F2, F3, F4},
	model.EnvUnknown:    {F0, F1, F2, F3, F4},
}

// FrictionFor 是纯函数：给定环境、风险与上下文，返回打扰强度。
//
// 规则（见 ADR-0004）：
//   - 只读连接/只读角色上的写操作直接 F5；
//   - R0 永远 F0；
//   - 变更集路径自带预览，摩擦降一级（F1 是下限，不再降）；
//   - 脚本过长、影响行数过大或未知、无近期备份，各 +1；
//   - 最高 F4，F5 只用于阻断。
func FrictionFor(env model.Environment, risk Level, m Modifiers) Friction {
	if risk >= R1 && (m.ReadOnlyConnection || m.ReadOnlyRole) {
		return F5
	}
	if risk == R0 {
		return F0
	}

	f := baseFriction[env][risk]

	if m.ChangeSetPath && f > F1 {
		f--
	}
	if m.StatementCount > 5 {
		f++
	}
	if m.RowsAffected > 10000 {
		f++
	}
	if m.RowsAffected < 0 { // 负值表示影响行数未知
		f++
	}
	if m.NoRecentBackup {
		f++
	}

	if f < F0 {
		f = F0
	}
	if f > F4 {
		f = F4
	}
	return f
}

// Explain 返回人类可读的分级说明，用于 UI 与审计。
func Explain(env model.Environment, risk Level, f Friction) string {
	return fmt.Sprintf("环境=%s 风险=%s 打扰=%s", env, risk, f)
}
