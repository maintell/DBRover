package agent

import (
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
)

// writeWithoutApproval 是「会改数据但可以不经过人工审批」的工具白名单。
//
// 现在是空的：change.apply 是 R2，必须人批。留这个变量是为了让「新增一个
// R1 写工具」必须显式声明，而不是悄悄把审批绕过去（docs/11 §5.2）。
var writeWithoutApproval = map[string]bool{}

// TestWriteToolsMatchContract 钉住写工具的风险等级。
//
// 风险等级决定要不要人批：R2 要，R1 不要。所以「把 R2 改成 R1」不是重构，
// 是取消审批——必须让它在测试里红，而不是靠 reviewer 眼力。
func TestWriteToolsMatchContract(t *testing.T) {
	want := map[string]policy.Level{
		"change.apply": policy.R2,
	}
	tools := (&Runtime{}).buildTools()
	for name, lvl := range want {
		tool, okTool := tools[name]
		if !okTool {
			t.Fatalf("契约里的写工具 %s 不在注册表里", name)
		}
		if !tool.Write {
			t.Errorf("%s 是写工具，却没标 Write：只读连接上的门禁会漏过它", name)
		}
		if tool.Risk != lvl {
			t.Errorf("%s 的风险等级应为 %s（docs/11 §5.2），实际 %s", name, lvl, tool.Risk)
		}
	}
}

// TestEveryWriteToolIsGated 把「写操作 100% 过门禁」变成自我强制的检查。
//
// 它遍历**真实注册表**，不认任何手写清单：以后加一个 Write 工具，或者把
// 只读判定改坏，这里就会红。docs/11 验收第 2 条靠它兜底。
func TestEveryWriteToolIsGated(t *testing.T) {
	r := &Runtime{}
	tools := r.buildTools()
	if len(tools) != 14 {
		t.Fatalf("工具数量应为 14（docs/11 §3.2），实际 %d——重名会让某个工具在这里悄悄消失", len(tools))
	}

	writes := 0
	for name, tool := range tools {
		if tool.Name != name {
			t.Errorf("工具注册名与自报名不一致：map=%s, Tool.Name=%s", name, tool.Name)
		}
		if !tool.Write {
			continue
		}
		writes++

		if tool.Risk < policy.R1 {
			t.Errorf("%s 标了 Write 却是 %s：会改数据就至少是 R1", tool.Name, tool.Risk)
		}
		// 写工具的风险等级本身是契约：把 change.apply 从 R2 降到 R1 就等于
		// 取消审批（R1 直接放行），这种「静默降级」必须被拦住。
		if tool.Risk < policy.R2 && !writeWithoutApproval[tool.Name] {
			t.Errorf("%s 是 %s 写工具，却不在免审批白名单里：新增写工具默认必须走人工审批", tool.Name, tool.Risk)
		}

		steps := []struct {
			why           string
			p             *Policy
			allowed       bool
			needsApproval bool
		}{
			{
				why:     "只读连接",
				p:       NewPolicy(ModeWriteWithApproval, model.EnvTest, model.AccessReadOnly),
				allowed: false,
			},
			{
				why:     "只读模式",
				p:       NewPolicy(ModeReadOnly, model.EnvTest, model.AccessReadWrite),
				allowed: false,
			},
			{
				why:           "写模式（按风险审批）",
				p:             NewPolicy(ModeWriteWithApproval, model.EnvTest, model.AccessReadWrite),
				allowed:       true,
				needsApproval: tool.Risk >= policy.R2,
			},
			{
				why:     "自主模式（test 环境）",
				p:       NewPolicy(ModeAutonomous, model.EnvTest, model.AccessReadWrite),
				allowed: true,
			},
		}
		for _, s := range steps {
			d := s.p.Authorize(tool)
			if d.Allowed != s.allowed || d.NeedsApproval != s.needsApproval {
				t.Errorf("%s 在%s下：allowed=%v needs_approval=%v，期望 %v/%v（%s）",
					tool.Name, s.why, d.Allowed, d.NeedsApproval, s.allowed, s.needsApproval, d.Reason)
			}
		}
	}

	if writes == 0 {
		t.Fatal("一个 Write 工具都没有：门禁测试等于没跑")
	}
}

// TestAutonomousIsClampedOnProduction 锁住 docs/11 §5.1 那条硬约束：
// 配置写了 autonomous、连的却是生产库，必须收窄成逐步审批，而不是照配置执行。
func TestAutonomousIsClampedOnProduction(t *testing.T) {
	p := NewPolicy(ModeAutonomous, model.EnvProduction, model.AccessReadWrite)
	if p.Mode() != ModeWriteWithApproval {
		t.Fatalf("生产库上的自主模式应被收窄为 %s，实际 %s", ModeWriteWithApproval, p.Mode())
	}
	d := p.Authorize(Tool{Name: "ddl.execute", Risk: policy.R3, Write: true})
	if !d.Allowed || !d.NeedsApproval {
		t.Fatalf("R3 写操作在生产库上必须「放行但要人批」，实际 allowed=%v needs_approval=%v（%s）",
			d.Allowed, d.NeedsApproval, d.Reason)
	}
}
