package policy

import (
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestFrictionMatrix 穷举「环境 × 风险」基准矩阵（ADR-0004 §5）。
func TestFrictionMatrix(t *testing.T) {
	want := map[model.Environment][5]Friction{
		model.EnvLocal:      {F0, F0, F0, F1, F2},
		model.EnvTest:       {F0, F0, F1, F2, F3},
		model.EnvStaging:    {F0, F1, F2, F3, F3},
		model.EnvProduction: {F0, F1, F2, F3, F4},
		model.EnvUnknown:    {F0, F1, F2, F3, F4},
	}

	levels := []Level{R0, R1, R2, R3, R4}
	for env, row := range want {
		for i, lv := range levels {
			got := FrictionFor(env, lv, Modifiers{})
			if got != row[i] {
				t.Errorf("env=%s risk=%s: got %s, want %s", env, lv, got, row[i])
			}
		}
	}
}

func TestUnknownEqualsProduction(t *testing.T) {
	for _, lv := range []Level{R0, R1, R2, R3, R4} {
		prod := FrictionFor(model.EnvProduction, lv, Modifiers{})
		unk := FrictionFor(model.EnvUnknown, lv, Modifiers{})
		if prod != unk {
			t.Errorf("risk=%s: unknown=%s 应等同 production=%s", lv, unk, prod)
		}
	}
}

func TestFrictionReadOnlyBlocks(t *testing.T) {
	if got := FrictionFor(model.EnvTest, R1, Modifiers{ReadOnlyConnection: true}); got != F5 {
		t.Errorf("只读连接上的 R1 应为 F5, got %s", got)
	}
	if got := FrictionFor(model.EnvLocal, R3, Modifiers{ReadOnlyRole: true}); got != F5 {
		t.Errorf("只读角色上的 R3 应为 F5, got %s", got)
	}
	if got := FrictionFor(model.EnvProduction, R0, Modifiers{ReadOnlyConnection: true}); got != F0 {
		t.Errorf("只读读写操作不应被阻断, got %s", got)
	}
}

func TestFrictionChangeSetPath(t *testing.T) {
	cases := []struct {
		env  model.Environment
		risk Level
		want Friction
	}{
		{model.EnvProduction, R2, F1}, // F2 - 1
		{model.EnvProduction, R3, F2}, // F3 - 1
		{model.EnvProduction, R1, F1}, // F1 不再降
		{model.EnvTest, R2, F1},       // F1 不再降
		{model.EnvTest, R1, F0},       // 本来就是 F0
		{model.EnvLocal, R2, F0},
	}
	for _, c := range cases {
		got := FrictionFor(c.env, c.risk, Modifiers{ChangeSetPath: true})
		if got != c.want {
			t.Errorf("env=%s risk=%s 变更集: got %s, want %s", c.env, c.risk, got, c.want)
		}
	}
}

func TestFrictionEscalators(t *testing.T) {
	base := FrictionFor(model.EnvTest, R2, Modifiers{})

	if got := FrictionFor(model.EnvTest, R2, Modifiers{StatementCount: 6}); got != base+1 {
		t.Errorf("语句数 > 5 应 +1: got %s, want %s", got, base+1)
	}
	if got := FrictionFor(model.EnvTest, R2, Modifiers{StatementCount: 5}); got != base {
		t.Errorf("语句数 <= 5 不应提升: got %s", got)
	}
	if got := FrictionFor(model.EnvTest, R2, Modifiers{RowsAffected: 20000}); got != base+1 {
		t.Errorf("影响行数 > 10000 应 +1: got %s", got)
	}
	if got := FrictionFor(model.EnvTest, R2, Modifiers{RowsAffected: -1}); got != base+1 {
		t.Errorf("影响行数未知应 +1: got %s", got)
	}
	if got := FrictionFor(model.EnvTest, R2, Modifiers{NoRecentBackup: true}); got != base+1 {
		t.Errorf("无近期备份应 +1: got %s", got)
	}
}

func TestFrictionCappedAtF4(t *testing.T) {
	got := FrictionFor(model.EnvProduction, R4, Modifiers{
		StatementCount: 20,
		RowsAffected:   1_000_000,
		NoRecentBackup: true,
	})
	if got != F4 {
		t.Errorf("摩擦上限应为 F4, got %s", got)
	}
}

func TestFrictionNoopForReads(t *testing.T) {
	got := FrictionFor(model.EnvProduction, R0, Modifiers{
		StatementCount: 20,
		RowsAffected:   -1,
		NoRecentBackup: true,
	})
	if got != F0 {
		t.Errorf("R0 不应被任何修正项提升, got %s", got)
	}
}

func TestFrictionPredicates(t *testing.T) {
	if F0.RequiresApproval() {
		t.Error("F0 不应要求确认")
	}
	if F1.RequiresApproval() {
		t.Error("F1 是内联预览，不算额外确认")
	}
	if !F2.RequiresApproval() {
		t.Error("F2 起应要求确认（与 service 执行门一致）")
	}
	if !F5.Blocked() {
		t.Error("F5 应被识别为阻断")
	}
	if F4.Blocked() {
		t.Error("F4 不是阻断")
	}
}

// TestRelaxRequiresExplicitDeclaration 断言「放宽必须显式声明，收紧可自动」。
func TestRelaxRequiresExplicitDeclaration(t *testing.T) {
	if !model.EnvProduction.CanRelax(model.EnvTest) {
		t.Error("production -> test 属于放宽，应需要确认")
	}
	if model.EnvTest.CanRelax(model.EnvProduction) {
		t.Error("test -> production 属于收紧，不应需要确认")
	}
	if model.EnvUnknown.CanRelax(model.EnvProduction) {
		t.Error("unknown 与 production 严格度相同，不算放宽")
	}
	if !model.EnvUnknown.CanRelax(model.EnvStaging) {
		t.Error("unknown -> staging 属于放宽")
	}
}
