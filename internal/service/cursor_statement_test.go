package service_test

import (
	"context"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestExecuteRunsOnlyStatementAtCursor 是端到端的那一条：
// 三段脚本、光标在中间那条，只有中间那条被执行，前后两条都没生效。
func TestExecuteRunsOnlyStatementAtCursor(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)

	script := `UPDATE t SET v = 'first' WHERE id = 1;
SELECT v FROM t WHERE id = 1;
UPDATE t SET v = 'third' WHERE id = 1;`

	// 光标落在第二句内部（"SELECT" 前面一点）。
	cursor := len("UPDATE t SET v = 'first' WHERE id = 1;\n") + 2
	out, err := ex.Execute(context.Background(), service.Request{SQL: script, Cursor: &cursor})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Err != nil {
		t.Fatalf("执行报错: %+v", out.Err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("只应执行一条，实际 %d 条", len(out.Results))
	}
	if got := out.Results[0].SQL; got != "SELECT v FROM t WHERE id = 1" {
		t.Errorf("执行的是 %q", got)
	}
	if v := e.value(t, 1); v != "a" {
		t.Errorf("前后的 UPDATE 都不该生效，v = %q", v)
	}
}

// TestExecuteWithoutCursorRunsWholeScript 反向断言：不传 Cursor 时仍是整段。
//
// 这条防的是「为了只跑一条，把所有调用方都改成只跑第一条」——
// 零值必须保持旧语义。
func TestExecuteWithoutCursorRunsWholeScript(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)

	out, err := ex.Execute(context.Background(),
		service.Request{SQL: "SELECT 1 AS a; SELECT 2 AS b;"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("应执行两条，实际 %d 条", len(out.Results))
	}
}

// TestCursorBetweenStatementsRunsPrevious 对应 docs/05 验收 2：
// 光标停在两段之间的空行上，跑的是前一段。
func TestCursorBetweenStatementsRunsPrevious(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)

	first := "UPDATE t SET v = 'first' WHERE id = 1"
	script := first + ";\n\nSELECT 1 AS x;"
	cursor := len(first) + 2 // 分号之后、空行里

	out, err := ex.Execute(context.Background(), service.Request{SQL: script, Cursor: &cursor})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("只应执行一条，实际 %d 条", len(out.Results))
	}
	if out.Results[0].SQL != first {
		t.Errorf("执行的是 %q，期望前一条", out.Results[0].SQL)
	}
	if v := e.value(t, 1); v != "first" {
		t.Errorf("应该执行了第一条 UPDATE，v = %q", v)
	}
}
