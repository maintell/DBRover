package agent_test

import (
	"testing"

	"github.com/maintell/DBRover/internal/agent"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
)

// TestClampModeMatrix 锁住 docs/11 §5.1 的环境上限表：
// 配置里写什么不算数，环境说了算。
func TestClampModeMatrix(t *testing.T) {
	cases := []struct {
		want agent.Mode
		env  model.Environment
		got  agent.Mode
	}{
		{agent.ModeAutonomous, model.EnvLocal, agent.ModeAutonomous},
		{agent.ModeAutonomous, model.EnvTest, agent.ModeAutonomous},
		{agent.ModeAutonomous, model.EnvStaging, agent.ModeWriteWithApproval},
		{agent.ModeAutonomous, model.EnvProduction, agent.ModeWriteWithApproval},
		{agent.ModeAutonomous, model.EnvUnknown, agent.ModeWriteWithApproval},
		{agent.ModeWriteWithApproval, model.EnvProduction, agent.ModeWriteWithApproval},
		{agent.ModeReadOnly, model.EnvLocal, agent.ModeReadOnly},
	}
	for _, c := range cases {
		if got := agent.ClampMode(c.want, c.env); got != c.got {
			t.Errorf("ClampMode(%s, %s) = %s，期望 %s", c.want, c.env, got, c.got)
		}
	}
}

// TestAuthorizeMatrix 是权限判定的穷举：模式 × 连接访问模式 × 工具风险。
func TestAuthorizeMatrix(t *testing.T) {
	readTool := agent.Tool{Name: "meta.objects", Risk: policy.R0}
	planTool := agent.Tool{Name: "change.plan", Risk: policy.R1, Write: true}
	applyTool := agent.Tool{Name: "change.apply", Risk: policy.R2, Write: true}

	cases := []struct {
		name         string
		mode         agent.Mode
		env          model.Environment
		access       model.AccessMode
		tool         agent.Tool
		wantAllowed  bool
		wantApproval bool
	}{
		// 只读模式：只放 R0
		{"只读模式放查询", agent.ModeReadOnly, model.EnvProduction, model.AccessReadWrite, readTool, true, false},
		{"只读模式拦写", agent.ModeReadOnly, model.EnvProduction, model.AccessReadWrite, planTool, false, false},

		// 审批模式：写要批
		{"审批模式 R1 直接放行", agent.ModeWriteWithApproval, model.EnvStaging, model.AccessReadWrite, planTool, true, false},
		{"审批模式 R2 要审批", agent.ModeWriteWithApproval, model.EnvStaging, model.AccessReadWrite, applyTool, true, true},

		// 自主模式：写也放行（只有 local/test 能被收窄到这里）
		{"自主模式 R2 不审批", agent.ModeAutonomous, model.EnvTest, model.AccessReadWrite, applyTool, true, false},

		// 连接本身只读：任何写都不给，与 Agent 模式无关
		{"连接只读拦住写入", agent.ModeAutonomous, model.EnvLocal, model.AccessReadOnly, planTool, false, false},

		// 生产 + 配置成自主：收窄成审批模式，写仍要批
		{"生产环境自主被收窄", agent.ModeAutonomous, model.EnvProduction, model.AccessReadWrite, applyTool, true, true},
	}

	for _, c := range cases {
		p := agent.NewPolicy(c.mode, c.env, c.access)
		d := p.Authorize(c.tool)
		if d.Allowed != c.wantAllowed {
			t.Errorf("%s：Allowed = %v，期望 %v（原因：%s）", c.name, d.Allowed, c.wantAllowed, d.Reason)
		}
		if c.wantAllowed && d.NeedsApproval != c.wantApproval {
			t.Errorf("%s：NeedsApproval = %v，期望 %v", c.name, d.NeedsApproval, c.wantApproval)
		}
		if !c.wantAllowed && d.Reason == "" {
			t.Errorf("%s：拒绝时必须给出原因", c.name)
		}
	}
}

// TestParseMode 空串默认只读，未知值报错而不是猜。
func TestParseMode(t *testing.T) {
	if m, err := agent.ParseMode(""); err != nil || m != agent.ModeReadOnly {
		t.Errorf("空串应解析为只读，实际 %v / %v", m, err)
	}
	if _, err := agent.ParseMode("yolo"); err == nil {
		t.Error("未知模式必须报错")
	}
}
