package service_test

import (
	"context"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
	"github.com/maintell/DBRover/internal/service"
)

// TestOrConditionCannotBypassApproval 是安全回归：
// 曾经 `WHERE id=1 OR 1=1` 语法上有等值，被判成 R1，
// 于是在生产连接上以 F1 零确认执行了全表更新。
func TestOrConditionCannotBypassApproval(t *testing.T) {
	e := setup(t)
	seed(t, e, 2, 2) // t 表现在有 id=1,2,3

	out, err := service.NewExecutor(e.rw, model.EnvProduction, model.AccessReadWrite, nil).
		Execute(context.Background(), service.Request{
			SQL: `UPDATE t SET v='pwned' WHERE id=1 OR 1=1`,
		})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Blocked {
		t.Fatalf("恒真 OR 条件必须被拦截，实际放行（risk=%s friction=%s）", out.Risk, out.Friction)
	}
	if out.Risk != policy.R4 {
		t.Errorf("Risk = %s, 期望 R4（等价于全表写）", out.Risk)
	}
	if got := e.value(t, 2); got == "pwned" {
		t.Error("被拦截后数据不应变化")
	}
}

// TestDisjunctionNeverGradesAsPrimaryKey 断言含 OR/NOT 的条件一律不判 R1。
func TestDisjunctionNeverGradesAsPrimaryKey(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)

	cases := []string{
		`UPDATE t SET v='x' WHERE id=1 OR name='a'`,
		`DELETE FROM t WHERE NOT id=1`,
	}
	for _, sql := range cases {
		out, err := ex.Execute(context.Background(), service.Request{SQL: sql})
		if err != nil {
			t.Fatalf("Execute(%q): %v", sql, err)
		}
		if out.Risk == policy.R1 {
			t.Errorf("%q 被判成 R1，含 OR/NOT 时不应命中主键降级", sql)
		}
	}
}

// TestReadOnlyModeVariantsAllBlock 覆盖「访问模式写法不同但都应只读」的情况。
func TestReadOnlyModeVariantsAllBlock(t *testing.T) {
	e := setup(t)

	for _, mode := range []model.AccessMode{model.AccessReadOnly, "", "bogus"} {
		ex := service.NewExecutor(e.rw, model.EnvProduction, mode, nil)
		out, err := ex.Execute(context.Background(), service.Request{
			SQL: `DELETE FROM t WHERE id=1`,
		})
		if err != nil {
			t.Fatalf("Execute(mode=%q): %v", mode, err)
		}
		if !out.Blocked {
			t.Errorf("mode=%q 时应拒绝写入，实际放行", mode)
		}
	}
}

// TestCursorIDsAreGloballyUnique 是串库回归：
// 两个连接各自的执行器曾用自增序号，都产生 "cur-1"，
// 导致 A 的游标翻出 B 的数据。
func TestCursorIDsAreGloballyUnique(t *testing.T) {
	e := setup(t)
	seed(t, e, 2, 3)

	ex1 := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	ex2 := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex1.Close()
	defer ex2.Close()

	a, err := ex1.Query(context.Background(), service.Request{SQL: `SELECT id FROM t`})
	if err != nil {
		t.Fatalf("Query1: %v", err)
	}
	b, err := ex2.Query(context.Background(), service.Request{SQL: `SELECT id FROM t`})
	if err != nil {
		t.Fatalf("Query2: %v", err)
	}

	if a.CursorID == b.CursorID {
		t.Fatalf("两个连接的游标 ID 相同: %s", a.CursorID)
	}
}
