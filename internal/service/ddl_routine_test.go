package service_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestDDLCreateRoutineRejectedEngines 钉住「没有例程就别说能建」。
//
// SQLite 没有存储过程与函数；生成 CREATE PROCEDURE 只会得到一句必然失败的语句，
// 而且失败信息不会告诉用户「换条路走」。
func TestDDLCreateRoutineRejectedEngines(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	_, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table: tableRef("rt"), Action: service.DDLCreateRoutine,
		Routine: service.RoutineSpec{Name: "rt_a", Body: "BEGIN SELECT 1; END"},
	})
	if !errors.Is(err, service.ErrInvalidDDL) {
		t.Fatalf("SQLite 上建例程应被拒, 实际 %v", err)
	}
	if !strings.Contains(err.Error(), "没有例程") {
		t.Errorf("要说明这个引擎没有例程：%v", err)
	}
}

// TestDDLCreateRoutineRejectsBadInput 覆盖参数校验（与引擎无关的部分）。
//
// 这些错在哪台引擎上都不成立，所以必须比「引擎不支持」先报出来。
func TestDDLCreateRoutineRejectsBadInput(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	cases := []struct {
		name string
		spec service.RoutineSpec
		want string
	}{
		{"名字非法", service.RoutineSpec{Name: "rt a", Body: "SELECT 1"}, "字母、数字、下划线"},
		{"种类非法", service.RoutineSpec{Name: "rt_a", Kind: "macro", Body: "SELECT 1"}, "只接受 procedure / function"},
		{"体为空", service.RoutineSpec{Name: "rt_a", Body: "   "}, "例程体不能为空"},
		{"体以分号结尾", service.RoutineSpec{Name: "rt_a", Body: "SELECT 1;"}, "不要以分号结尾"},
		{"体含定界符", service.RoutineSpec{Name: "rt_a", Body: "SELECT '$dbrover$"}, "不能出现"},
		{"函数缺返回类型", service.RoutineSpec{Name: "rt_a", Kind: "function", Body: "SELECT 1"}, "必须给返回类型"},
		{"过程给了返回类型", service.RoutineSpec{Name: "rt_a", Returns: "integer", Body: "SELECT 1"}, "没有返回类型"},
		{"返回类型非法", service.RoutineSpec{Name: "rt_a", Kind: "function", Returns: "int; DROP", Body: "SELECT 1"}, "不在白名单"},
		{"参数类型非法", service.RoutineSpec{Name: "rt_a", Body: "SELECT 1",
			Params: []service.RoutineParam{{Name: "a", Type: "int; DROP"}}}, "不在白名单"},
		{"参数名重复", service.RoutineSpec{Name: "rt_a", Body: "SELECT 1",
			Params: []service.RoutineParam{{Name: "a", Type: "int"}, {Name: "A", Type: "text"}}}, "重复"},
		{"参数名为空", service.RoutineSpec{Name: "rt_a", Body: "SELECT 1",
			Params: []service.RoutineParam{{Name: "", Type: "int"}}}, "参数名"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("rt"), Action: service.DDLCreateRoutine, Routine: c.spec,
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

// TestDDLCreateRoutineOnRealEngines 走完整链路：出预案 → 执行 → 真能调用 →
// 回滚 → 调用失败。两引擎的回滚语句形态不同（PG 要带签名），都得断言。
func TestDDLCreateRoutineOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		tgt := tgt
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			body, drop, call := "BEGIN SELECT 1; END", "DROP PROCEDURE IF EXISTS it_proc", "CALL it_proc()"
			if tgt.name == "pg" {
				// PG 的 DROP 要带签名；体里不能只有 SELECT（过程没有返回值）。
				body, drop = "BEGIN NULL; END", "DROP PROCEDURE IF EXISTS it_proc()"
			}
			_ = execStrict(ctx, ex, drop)

			plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("it_proc"), Action: service.DDLCreateRoutine,
				Routine: service.RoutineSpec{Name: "it_proc", Body: body},
			})
			if err != nil {
				t.Fatalf("生成预案: %v", err)
			}
			if plan.Table.Kind != model.KindRoutine {
				t.Fatalf("预案目标应是例程: %+v", plan.Table)
			}
			if !strings.Contains(plan.Statements[0].SQL, "CREATE PROCEDURE") {
				t.Fatalf("语句不对：%s", plan.Statements[0].SQL)
			}
			if out, aerr := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行建例程: %v %+v", aerr, out)
			}
			// 建出来的例程真能调用。
			if err := execStrict(ctx, ex, call); err != nil {
				t.Fatalf("调用例程: %v", err)
			}

			// 回滚语句的形态：PG 必须带空签名，否则删不掉。
			if tgt.name == "pg" && !strings.Contains(plan.Rollback[0].SQL, "()") {
				t.Fatalf("PG 的 DROP PROCEDURE 必须带签名：%s", plan.Rollback[0].SQL)
			}
			for _, s := range plan.Rollback {
				if err := execStrict(ctx, ex, s.SQL); err != nil {
					t.Fatalf("回滚（%s）: %v", s.SQL, err)
				}
			}
			if err := execStrict(ctx, ex, call); err == nil {
				t.Fatal("回滚后例程应当不存在")
			}

			switch tgt.name {
			case "pg":
				// 函数这条路：返回类型 + 参数 + 语言。
				_ = execStrict(ctx, ex, "DROP FUNCTION IF EXISTS it_fn(integer)")
				fplan, ferr := ex.BuildDDLPlan(ctx, service.DDLRequest{
					Table: tableRef("it_fn"), Action: service.DDLCreateRoutine,
					Routine: service.RoutineSpec{
						Name: "it_fn", Kind: "function", Returns: "integer", Language: "sql",
						Params: []service.RoutineParam{{Name: "a", Type: "integer"}},
						Body:   "SELECT a * 2",
					},
				})
				if ferr != nil {
					t.Fatalf("生成建函数预案: %v", ferr)
				}
				if !strings.Contains(fplan.Statements[0].SQL, "RETURNS integer") ||
					!strings.Contains(fplan.Statements[0].SQL, "LANGUAGE sql") {
					t.Fatalf("函数语句缺返回类型或语言：%s", fplan.Statements[0].SQL)
				}
				if out, aerr := ex.ApplyChangePlan(ctx, fplan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
					t.Fatalf("执行建函数: %v %+v", aerr, out)
				}
				rows := queryRows(t, ctx, ex, "SELECT it_fn(21)")
				if len(rows) != 1 || fmt.Sprint(rows[0][0]) != "42" {
					t.Fatalf("it_fn(21) 应返回 42，实际 %v", rows)
				}
				// 名字会被引用成 "it_fn"，所以按「类型签名 + 关键字」断言。
				if !strings.Contains(fplan.Rollback[0].SQL, "DROP FUNCTION") ||
					!strings.Contains(fplan.Rollback[0].SQL, "(integer)") {
					t.Fatalf("函数的回滚必须带参数类型：%s", fplan.Rollback[0].SQL)
				}
				for _, s := range fplan.Rollback {
					if err := execStrict(ctx, ex, s.SQL); err != nil {
						t.Fatalf("回滚函数（%s）: %v", s.SQL, err)
					}
				}
			case "mysql":
				// 函数被明确拒绝，而不是生成一条在开 binlog 的实例上必然失败的语句。
				_, ferr := ex.BuildDDLPlan(ctx, service.DDLRequest{
					Table: tableRef("it_fn"), Action: service.DDLCreateRoutine,
					Routine: service.RoutineSpec{Name: "it_fn", Kind: "function", Returns: "int", Body: "RETURN 1"},
				})
				if !errors.Is(ferr, service.ErrInvalidDDL) {
					t.Fatalf("MySQL 上建函数应被拒, 实际 %v", ferr)
				}
				if !strings.Contains(ferr.Error(), "1418") {
					t.Errorf("要说明真正的原因（binlog 下要求 DETERMINISTIC 等）：%v", ferr)
				}
			}
		})
	}
}
