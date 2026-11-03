package agent

import (
	"fmt"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
)

// Mode 是 Agent 的权限模式（docs/11 §5.1）。
//
// 它是**人类设置项**：Agent 无法修改自己的模式，也没有任何工具能改它。
type Mode string

const (
	// ModeReadOnly 只允许 R0 工具，任何写操作一律拒绝。生产/预发默认。
	ModeReadOnly Mode = "read-only"
	// ModeWriteWithApproval 允许全部工具，写操作按风险逐次审批。
	ModeWriteWithApproval Mode = "write-with-approval"
	// ModeAutonomous 按策略自动放行写操作，只能用于 local / test。
	ModeAutonomous Mode = "autonomous"
)

// ParseMode 解析模式文本，空串视为只读（默认最保守）。
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "":
		return ModeReadOnly, nil
	case ModeReadOnly:
		return ModeReadOnly, nil
	case ModeWriteWithApproval:
		return ModeWriteWithApproval, nil
	case ModeAutonomous:
		return ModeAutonomous, nil
	default:
		return "", fmt.Errorf("agent: 未知权限模式 %q（可用 read-only / write-with-approval / autonomous）", s)
	}
}

// ClampMode 按环境上限收窄模式。
//
// 这一条是硬约束而不是建议：配置里写了 autonomous 但连的是生产库，
// 收窄到 write-with-approval 并且要说清楚原因，而不是照配置执行。
func ClampMode(want Mode, env model.Environment) Mode {
	if want == ModeAutonomous && !autonomousAllowed(env) {
		return ModeWriteWithApproval
	}
	return want
}

// autonomousAllowed 判断某环境是否允许 Agent 自主写。
func autonomousAllowed(env model.Environment) bool {
	return env == model.EnvLocal || env == model.EnvTest
}

// Decision 是一次授权判定的结论。
type Decision struct {
	Allowed bool
	// NeedsApproval 表示可以执行，但必须先由人确认这次具体动作。
	NeedsApproval bool
	// Reason 说明为什么被拒或为什么需要审批，直接进工具返回的信封。
	Reason string
}

// Policy 把「模式 × 环境 × 连接访问模式 × 工具风险」收敛成一次判定。
//
// 判定集中在一个地方（docs/11 §5.3）：散落在各个工具里，一定会出现
// 「这个工具忘了检查」的漏洞，而那种漏洞不会报错，只会静默放行。
type Policy struct {
	mode   Mode
	env    model.Environment
	access model.AccessMode
}

// NewPolicy 构造判定器。mode 会被环境上限收窄。
func NewPolicy(mode Mode, env model.Environment, access model.AccessMode) *Policy {
	if env == "" {
		env = model.EnvUnknown
	}
	if access == "" {
		access = model.DefaultAccessMode(env)
	}
	return &Policy{mode: ClampMode(mode, env), env: env, access: access}
}

// Mode 返回收窄后的实际模式。
func (p *Policy) Mode() Mode { return p.mode }

// Authorize 判定一次工具调用。
func (p *Policy) Authorize(t Tool) Decision {
	// 1. 连接只读时，任何写路径都不给——这是连接本身的属性，不是 Agent 的选择。
	if t.Write && p.access == model.AccessReadOnly {
		return Decision{Reason: "这条连接是只读模式，Agent 不能写入"}
	}

	// 2. 只读模式只放 R0。
	if p.mode == ModeReadOnly && t.Risk > policy.R0 {
		return Decision{
			Reason: fmt.Sprintf("当前是只读模式，只允许 R0 工具；%s 是 %s", t.Name, t.Risk),
		}
	}

	// 3. 写操作在非自主模式下按风险分级要求审批。
	if t.Write && p.mode != ModeAutonomous && t.Risk >= policy.R2 {
		return Decision{
			Allowed:       true,
			NeedsApproval: true,
			Reason:        fmt.Sprintf("%s 是 %s，需要人工确认后才能执行", t.Name, t.Risk),
		}
	}

	// 4. 自主模式也不能自己发 DDL 到 staging 以上——收窄已经在 ClampMode 里做过；
	//    这里再挡一次，因为 ClampMode 只作用于「配置想要什么」，不作用于「现在在做什么」。
	if t.Risk >= policy.R3 && !autonomousAllowed(p.env) && p.mode == ModeAutonomous {
		return Decision{Reason: "这个环境不允许 Agent 自主执行结构变更"}
	}

	return Decision{Allowed: true}
}
