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

// TestDDLCreateView 覆盖建视图的三件事：能建、能回滚、参数错了当场拒。
//
// 用 SQLite 就够了：CREATE VIEW 在三种引擎上是同一句话，差异在方言之外。
// 引擎特有的部分（视图定义如何读回）由 drivertest 的依赖检查与比对用例覆盖。
func TestDDLCreateView(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLCreateView,
		View:   service.ViewSpec{Name: "v_t", Query: "SELECT id, v FROM t WHERE id > 0"},
	})
	if err != nil {
		t.Fatalf("生成建视图预案: %v", err)
	}
	if len(plan.Statements) != 1 || !strings.Contains(plan.Statements[0].SQL, "CREATE VIEW") {
		t.Fatalf("预案语句不对：%+v", plan.Statements)
	}
	if len(plan.Rollback) != 1 || !strings.Contains(strings.ToUpper(plan.Rollback[0].SQL), "DROP VIEW") {
		t.Fatalf("回滚应是 DROP VIEW：%+v", plan.Rollback)
	}
	if out, aerr := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
		t.Fatalf("执行建视图: %v %+v", aerr, out)
	}
	// 真建出来了：能看到视图对象，也能查它（不报错）。
	objs, err := e.rw.Meta().Objects(ctx, "", "", model.KindView)
	if err != nil {
		t.Fatalf("列对象: %v", err)
	}
	found := false
	for _, o := range objs {
		if strings.EqualFold(o.Name, "v_t") {
			found = true
		}
	}
	if !found {
		t.Fatalf("视图没建出来：%+v", objs)
	}
	if err := execStrict(ctx, ex, "SELECT * FROM v_t"); err != nil {
		t.Fatalf("视图查不了：%v", err)
	}
	// 回滚把它删掉。
	for _, s := range plan.Rollback {
		if err := execStrict(ctx, ex, s.SQL); err != nil {
			t.Fatalf("回滚（%s）: %v", s.SQL, err)
		}
	}
	if err := execStrict(ctx, ex, "SELECT * FROM v_t"); err == nil {
		t.Fatal("回滚后视图应当不存在")
	}
}

// TestDDLCreateViewRejectsBadInput 覆盖参数校验。
func TestDDLCreateViewRejectsBadInput(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	// 先建一个视图，用来验证「同名拒绝」。
	if _, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table: tableRef("t"), Action: service.DDLCreateView,
		View: service.ViewSpec{Name: "v_dup", Query: "SELECT 1 AS a"},
	}); err != nil {
		t.Fatalf("准备同名视图预案: %v", err)
	}
	if out, err := func() (*service.Outcome, error) {
		p, perr := ex.BuildDDLPlan(ctx, service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLCreateView,
			View: service.ViewSpec{Name: "v_dup", Query: "SELECT 1 AS a"},
		})
		if perr != nil {
			return nil, perr
		}
		return ex.ApplyChangePlan(ctx, p, service.Request{Approval: true})
	}(); err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("建 v_dup: %v %+v", err, out)
	}

	cases := []struct{ name, viewName, query, want string }{
		{"空定义", "v_ok", "   ", "不能为空"},
		{"不是查询", "v_ok", "DELETE FROM t", "SELECT 或 WITH"},
		{"带分号", "v_ok", "SELECT 1; DROP TABLE t", "分号"},
		{"名字非法", "v ok", "SELECT 1", "字母、数字、下划线"},
		{"同名已存在", "v_dup", "SELECT 1", "已存在"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("t"), Action: service.DDLCreateView,
				View: service.ViewSpec{Name: c.viewName, Query: c.query},
			})
			if !errors.Is(err, service.ErrInvalidDDL) {
				t.Fatalf("应返回 ErrInvalidDDL, 实际 %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误应包含 %q，实际 %v", c.want, err)
			}
		})
	}
}

// TestDDLCreateViewOnRealEngines 在真实引擎上走一遍：建视图 → 查得到 → 回滚 → 没了。
//
// SQLite 上过不代表 MySQL/PG 也过：执行门对 CREATE VIEW 的分级、以及 PG 的
// schema 限定，都可能在这里出问题。
func TestDDLCreateViewOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP VIEW IF EXISTS it_v",
				"DROP TABLE IF EXISTS it_vsrc",
				"CREATE TABLE it_vsrc (id INTEGER PRIMARY KEY, v VARCHAR(20))",
			}, func(ctx context.Context, s driver.Session) {
				_, _ = s.Execute(ctx, "INSERT INTO it_vsrc (id, v) VALUES (1, 'a')", nil)
			})
			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("it_vsrc"), Action: service.DDLCreateView,
				View: service.ViewSpec{Name: "it_v", Query: "SELECT id, v FROM it_vsrc WHERE id > 0"},
			})
			if err != nil {
				t.Fatalf("生成预案: %v", err)
			}
			if out, aerr := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行建视图: %v %+v", aerr, out)
			}
			if err := execStrict(ctx, ex, "SELECT id, v FROM it_v"); err != nil {
				t.Fatalf("视图查不了：%v", err)
			}
			for _, s := range plan.Rollback {
				if err := execStrict(ctx, ex, s.SQL); err != nil {
					t.Fatalf("回滚（%s）: %v", s.SQL, err)
				}
			}
			if err := execStrict(ctx, ex, "SELECT id, v FROM it_v"); err == nil {
				t.Fatal("回滚后视图应当不存在")
			}
		})
	}
}
