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

// TestDDLReplaceRoutineOnRealEngines 是这条功能的核心验收：替换之后新行为生效，
// **回滚之后旧行为回来**。
//
// 只断言「语句生成对了」是不够的——回滚用旧定义重建这件事，只有真跑过才知道
// 它到底恢不恢复得了。
//
// 两引擎的观测方式不同：PG 用一个函数，`SELECT it_fn(10)` 直接读返回值；MySQL
// 的过程不返回结果集给这条执行路径（CALL 被当作非查询），所以让过程往一张表里
// 插标记值，再读最后一行。
func TestDDLReplaceRoutineOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		tgt := tgt
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			var first, second service.DDLRequest
			var call, observe, wantOld, wantNew string
			switch tgt.name {
			case "pg":
				_ = execStrict(ctx, ex, "DROP FUNCTION IF EXISTS it_fn(integer)")
				first = service.DDLRequest{
					Table: tableRef("it_fn"), Action: service.DDLCreateRoutine,
					Routine: service.RoutineSpec{
						Name: "it_fn", Kind: "function", Returns: "integer", Language: "sql",
						Params: []service.RoutineParam{{Name: "a", Type: "integer"}},
						Body:   "SELECT a * 2",
					},
				}
				second = first
				second.Action = service.DDLReplaceRoutine
				second.Routine.Body = "SELECT a * 3"
				call, observe, wantOld, wantNew = "", "SELECT it_fn(10)", "20", "30"
			default:
				_ = execStrict(ctx, ex, "DROP PROCEDURE IF EXISTS it_proc")
				_ = execStrict(ctx, ex, "DROP TABLE IF EXISTS it_rt")
				if err := execStrict(ctx, ex, "CREATE TABLE it_rt (id INT PRIMARY KEY AUTO_INCREMENT, v VARCHAR(8))"); err != nil {
					t.Fatalf("建观测表: %v", err)
				}
				defer func() { _ = execStrict(context.Background(), ex, "DROP TABLE IF EXISTS it_rt") }()
				first = service.DDLRequest{
					Table: tableRef("it_proc"), Action: service.DDLCreateRoutine,
					Routine: service.RoutineSpec{Name: "it_proc", Body: "BEGIN INSERT INTO it_rt (v) VALUES ('old'); END"},
				}
				second = first
				second.Action = service.DDLReplaceRoutine
				second.Routine.Body = "BEGIN INSERT INTO it_rt (v) VALUES ('new'); END"
				call, observe, wantOld, wantNew = "CALL it_proc()", "SELECT v FROM it_rt ORDER BY id DESC LIMIT 1", "old", "new"
			}

			// observeNow 先调一次例程，再读它留下的痕迹。
			observeNow := func() string {
				t.Helper()
				if call != "" {
					if err := execStrict(ctx, ex, call); err != nil {
						t.Fatalf("调用例程（%s）: %v", call, err)
					}
				}
				return firstCell(t, ctx, ex, observe)
			}

			createPlan, err := ex.BuildDDLPlan(ctx, first)
			if err != nil {
				t.Fatalf("生成新建预案: %v", err)
			}
			if out, aerr := ex.ApplyChangePlan(ctx, createPlan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行新建: %v %+v", aerr, out)
			}
			if got := observeNow(); got != wantOld {
				t.Fatalf("新建后观测值 = %s，期望 %s", got, wantOld)
			}

			replacePlan, err := ex.BuildDDLPlan(ctx, second)
			if err != nil {
				t.Fatalf("生成替换预案: %v", err)
			}
			if replacePlan.Table.Kind != model.KindRoutine {
				t.Fatalf("预案目标应是例程: %+v", replacePlan.Table)
			}
			// 语句条数是引擎差异：PG 一条 OR REPLACE，MySQL 两条 DROP + CREATE。
			if tgt.name == "pg" {
				if len(replacePlan.Statements) != 1 || !strings.Contains(replacePlan.Statements[0].SQL, "CREATE OR REPLACE") {
					t.Fatalf("PG 应是单条 CREATE OR REPLACE：%+v", replacePlan.Statements)
				}
			} else if len(replacePlan.Statements) != 2 || !strings.Contains(replacePlan.Statements[0].SQL, "DROP") {
				t.Fatalf("MySQL 应是 DROP + CREATE 两条：%+v", replacePlan.Statements)
			}
			if len(replacePlan.Rollback) == 0 {
				t.Fatal("替换预案必须有回滚语句")
			}
			if out, aerr := ex.ApplyChangePlan(ctx, replacePlan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行替换: %v %+v", aerr, out)
			}
			if got := observeNow(); got != wantNew {
				t.Fatalf("替换后观测值 = %s，期望 %s", got, wantNew)
			}

			for _, s := range replacePlan.Rollback {
				if err := execStrict(ctx, ex, s.SQL); err != nil {
					t.Fatalf("回滚（%s）: %v", s.SQL, err)
				}
			}
			if got := observeNow(); got != wantOld {
				t.Fatalf("回滚后观测值 = %s，期望旧行为 %s", got, wantOld)
			}

			// 不存在时要说清「新建请用 create_routine」，而不是让引擎报错。
			missing := second
			missing.Routine.Name = "no_such_routine"
			if _, merr := ex.BuildDDLPlan(ctx, missing); !errors.Is(merr, service.ErrInvalidDDL) ||
				!strings.Contains(merr.Error(), "不存在") {
				t.Fatalf("替换不存在的例程应被拒并说明, 实际 %v", merr)
			}

			// 种类必须与现状一致（只有 PG 这段能造出「过程被当成函数替换」）。
			if tgt.name == "pg" {
				_ = execStrict(ctx, ex, "DROP PROCEDURE IF EXISTS it_mix()")
				pmk, perr := ex.BuildDDLPlan(ctx, service.DDLRequest{
					Table: tableRef("it_mix"), Action: service.DDLCreateRoutine,
					Routine: service.RoutineSpec{Name: "it_mix", Body: "BEGIN NULL; END"},
				})
				if perr != nil {
					t.Fatalf("生成建过程预案: %v", perr)
				}
				if out, aerr := ex.ApplyChangePlan(ctx, pmk, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
					t.Fatalf("执行建过程: %v %+v", aerr, out)
				}
				mix := second
				mix.Routine.Name = "it_mix"
				if _, merr := ex.BuildDDLPlan(ctx, mix); !errors.Is(merr, service.ErrInvalidDDL) ||
					!strings.Contains(merr.Error(), "存储过程") {
					t.Fatalf("种类不一致应被拒并说明, 实际 %v", merr)
				}
				_ = execStrict(ctx, ex, "DROP PROCEDURE IF EXISTS it_mix()")
			}
		})
	}
}

// firstCell 执行一条查询并取第一个结果集的第一个单元格，用来观测例程行为。
func firstCell(t *testing.T, ctx context.Context, ex *service.Executor, sql string) string {
	t.Helper()
	rows := queryRows(t, ctx, ex, sql)
	if len(rows) == 0 || len(rows[0]) == 0 {
		t.Fatalf("%q 没有返回单元格", sql)
	}
	return fmt.Sprint(rows[0][0])
}
