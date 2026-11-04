package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestDDLCreateMatViewRejectedEngines 钉住「没有物化视图就别说能建」。
func TestDDLCreateMatViewRejectedEngines(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	_, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table: tableRef("t"), Action: service.DDLCreateMatView,
		View: service.ViewSpec{Name: "mv_a", Query: "SELECT id FROM t"},
	})
	if !errors.Is(err, service.ErrInvalidDDL) {
		t.Fatalf("SQLite 上建物化视图应被拒, 实际 %v", err)
	}
	if !strings.Contains(err.Error(), "没有物化视图") {
		t.Errorf("要说明这个引擎没有这个概念：%v", err)
	}
}

// TestDDLCreateMatViewOnRealPG 在 PG 上验证物化视图：建得出来、**数据是旧的**、
// REFRESH 之后才更新、回滚能删。
//
// 「数据是旧的」这条是重点：它证明我们建的是真的物化视图，而不是被引擎当成
// 普通视图处理。用户最容易误解的就是这一点，所以它得有用例守着。
func TestDDLCreateMatViewOnRealPG(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		if tgt.name != "pg" {
			t.Skip("物化视图只有 PostgreSQL 有")
		}
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP MATERIALIZED VIEW IF EXISTS it_mv",
				"DROP TABLE IF EXISTS it_mvsrc",
				"CREATE TABLE it_mvsrc (id INTEGER PRIMARY KEY, v VARCHAR(20))",
			}, func(ctx context.Context, s driver.Session) {
				_, _ = s.Execute(ctx, "INSERT INTO it_mvsrc (id, v) VALUES (1, 'a')", nil)
			})
			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("it_mvsrc"), Action: service.DDLCreateMatView,
				View: service.ViewSpec{Name: "it_mv", Query: "SELECT id, v FROM it_mvsrc"},
			})
			if err != nil {
				t.Fatalf("生成预案: %v", err)
			}
			if !strings.Contains(plan.Statements[0].SQL, "CREATE MATERIALIZED VIEW") {
				t.Fatalf("语句不对：%s", plan.Statements[0].SQL)
			}
			if out, aerr := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行建物化视图: %v %+v", aerr, out)
			}
			if err := execStrict(ctx, ex, "SELECT id, v FROM it_mv"); err != nil {
				t.Fatalf("物化视图查不了：%v", err)
			}
			// 源表再插一行：物化视图**不该**跟着变。
			if err := execStrict(ctx, ex, "INSERT INTO it_mvsrc (id, v) VALUES (2, 'b')"); err != nil {
				t.Fatalf("插源表: %v", err)
			}
			if n := countRows(t, ctx, ex, "SELECT count(*) FROM it_mv"); n != 1 {
				t.Fatalf("物化视图的数据不该自动更新，实际 %d 行——这说明引擎把它当普通视图了", n)
			}
			if err := execStrict(ctx, ex, "REFRESH MATERIALIZED VIEW it_mv"); err != nil {
				t.Fatalf("REFRESH: %v", err)
			}
			if n := countRows(t, ctx, ex, "SELECT count(*) FROM it_mv"); n != 2 {
				t.Fatalf("REFRESH 后应有 2 行，实际 %d", n)
			}
			for _, s := range plan.Rollback {
				if err := execStrict(ctx, ex, s.SQL); err != nil {
					t.Fatalf("回滚（%s）: %v", s.SQL, err)
				}
			}
			if err := execStrict(ctx, ex, "SELECT id, v FROM it_mv"); err == nil {
				t.Fatal("回滚后物化视图应当不存在")
			}
		})
	}
}

// countRows 跑一条 count 查询并取第一个值。
func countRows(t *testing.T, ctx context.Context, ex *service.Executor, sql string) int64 {
	t.Helper()
	out, err := ex.Execute(ctx, service.Request{SQL: sql, Approval: true})
	if err != nil || out == nil || out.Err != nil {
		t.Fatalf("查询 %q 失败：%v %+v", sql, err, out)
	}
	if len(out.Results) != 1 || len(out.Results[0].Rows) != 1 {
		t.Fatalf("查询 %q 结果形状异常：%+v", sql, out.Results)
	}
	v := out.Results[0].Rows[0][0]
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	}
	t.Fatalf("count 结果不是整数：%T", v)
	return 0
}
