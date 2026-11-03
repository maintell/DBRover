package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestMultiStatementKeepsEarlierResults 对应 docs/05 验收 4：
// 多语句里第 2 条失败，第 1 条的结果必须留下来。
//
// 这里测的是执行器层：失败时就地返回，Results 里带着「成功的那条 + 失败的那条」，
// 第 3 条根本不执行（半途生效比全都不做更难收拾）。
func TestMultiStatementKeepsEarlierResults(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)

	out, err := ex.Execute(context.Background(), service.Request{
		SQL: "SELECT 1 AS a; SELECT * FROM no_such_table; SELECT 3 AS c;",
	})
	if err != nil {
		t.Fatalf("Execute 不该返回错误（结论在 Outcome 里）: %v", err)
	}
	if out.Err == nil {
		t.Fatal("第 2 条失败时必须给出结构化错误")
	}
	if len(out.Results) != 2 {
		t.Fatalf("应留下 2 条结果（成功 + 失败），实际 %d 条：%+v", len(out.Results), out.Results)
	}

	first := out.Results[0]
	if first.Err != "" {
		t.Fatalf("第 1 条应成功: %+v", first)
	}
	if len(first.Rows) != 1 || len(first.Rows[0]) != 1 {
		t.Fatalf("第 1 条应带回 1 行 1 列: %+v", first.Rows)
	}
	if got, _ := first.Rows[0][0].(int64); got != 1 {
		t.Errorf("第 1 条的值 = %v，期望 1", first.Rows[0][0])
	}

	if !strings.Contains(out.Results[1].Err, "no_such_table") {
		t.Errorf("第 2 条的错误应指向不存在的表: %q", out.Results[1].Err)
	}
	// 第 3 条不该执行：结果条数停在失败那一条。
	for _, r := range out.Results {
		if strings.Contains(r.SQL, "SELECT 3") {
			t.Errorf("失败之后不该继续执行: %+v", r)
		}
	}
}
