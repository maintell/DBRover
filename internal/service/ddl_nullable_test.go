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

// TestDDLSetNullableAndDefaultUnsupportedOnSQLite 钉住「做不到就明说」。
//
// SQLite 没有 ALTER COLUMN：改可空、改默认值都只能重建表。生成一条必然失败的
// 语句比拒绝更糟——用户会以为是自己数据的问题。
func TestDDLSetNullableAndDefaultUnsupportedOnSQLite(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	cases := []struct {
		name string
		req  service.DDLRequest
	}{
		{"改可空", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLSetNullable,
			Column: service.ColumnSpec{Name: "v", Nullable: false},
		}},
		{"改默认值", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLSetDefault,
			Column: service.ColumnSpec{Name: "v", Default: strPtr("'x'")},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ex.BuildDDLPlan(ctx, c.req)
			if !errors.Is(err, service.ErrInvalidDDL) {
				t.Fatalf("应返回 ErrInvalidDDL, 实际 %v", err)
			}
			if !strings.Contains(err.Error(), "不支持") {
				t.Errorf("错误要说清是引擎不支持：%v", err)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

// TestDDLSetNullableOnRealEngines 在真实 MySQL / PostgreSQL 上验证改可空：
// 有 NULL 时阻断并报出多少行；补好数据后出预案、执行、回滚能变回去。
func TestDDLSetNullableOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP TABLE IF EXISTS it_nullable",
				"CREATE TABLE it_nullable (id INTEGER PRIMARY KEY, v VARCHAR(20) NULL)",
			}, func(ctx context.Context, s driver.Session) {
				_, _ = s.Execute(ctx, "INSERT INTO it_nullable (id, v) VALUES (1, 'a')", nil)
				_, _ = s.Execute(ctx, "INSERT INTO it_nullable (id, v) VALUES (2, NULL)", nil)
			})

			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			// 1) 还有 NULL：必须在我们这一侧就拦住，并说清多少行。
			//    「交给引擎报错」拿不到行数，用户只能自己再写一条 SQL 去数。
			if _, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("it_nullable"), Action: service.DDLSetNullable,
				Column: service.ColumnSpec{Name: "v", Nullable: false},
			}); !errors.Is(err, service.ErrNullableBlocked) {
				t.Fatalf("有 NULL 时应返回 ErrNullableBlocked, 实际 %v", err)
			} else if !strings.Contains(err.Error(), "1 行") {
				t.Errorf("要报出多少行是 NULL：%v", err)
			}

			// 2) 补上值之后放行，并且语句真能执行。
			if _, err := ex.Execute(ctx, service.Request{
				SQL: "UPDATE it_nullable SET v = 'b' WHERE id = 2", Approval: true,
			}); err != nil {
				t.Fatalf("补数据: %v", err)
			}
			plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("it_nullable"), Action: service.DDLSetNullable,
				Column: service.ColumnSpec{Name: "v", Nullable: false},
			})
			if err != nil {
				t.Fatalf("生成预案: %v", err)
			}
			if len(plan.Statements) == 0 || len(plan.Rollback) == 0 {
				t.Fatalf("预案缺语句或回滚：%+v", plan)
			}
			if plan.AffectedRows != -1 {
				t.Errorf("改可空不写「0 行」也不该猜：期望 -1，实际 %d", plan.AffectedRows)
			}
			if out, aerr := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行改可空: %v %+v", aerr, out)
			}
			// 3) 回滚必须能变回去（可空 → 非空 是可逆的）。
			for _, s := range plan.Rollback {
				if out, rerr := ex.Execute(ctx, service.Request{SQL: s.SQL, Approval: true}); rerr != nil || out.Err != nil {
					t.Fatalf("回滚（%s）: %v %+v", s.SQL, rerr, out)
				}
			}
		})
	}
}

// TestDDLSetDefaultOnRealEngines 验证改默认值：字面量能改、表达式一律拒。
func TestDDLSetDefaultOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP TABLE IF EXISTS it_def",
				"CREATE TABLE it_def (id INTEGER PRIMARY KEY, n INTEGER DEFAULT 5)",
			}, nil)

			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			// 表达式默认值不是字面量，必须拒绝：它能塞进任意 SQL。
			if _, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("it_def"), Action: service.DDLSetDefault,
				Column: service.ColumnSpec{Name: "n", Default: strPtr("nextval('s')")},
			}); !errors.Is(err, service.ErrInvalidDDL) || !strings.Contains(err.Error(), "字面量") {
				t.Fatalf("表达式默认值应被拒，实际 %v", err)
			}

			plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("it_def"), Action: service.DDLSetDefault,
				Column: service.ColumnSpec{Name: "n", Default: strPtr("9")},
			})
			if err != nil {
				t.Fatalf("生成预案: %v", err)
			}
			if len(plan.Statements) == 0 || len(plan.Rollback) == 0 {
				t.Fatalf("预案缺语句或回滚：%+v", plan)
			}
			if !strings.Contains(plan.Statements[0].SQL, "SET DEFAULT 9") {
				t.Errorf("语句应是 SET DEFAULT 9：%s", plan.Statements[0].SQL)
			}
			if !strings.Contains(plan.Rollback[0].SQL, "SET DEFAULT 5") {
				t.Errorf("回滚应恢复原默认值 5：%s", plan.Rollback[0].SQL)
			}
			if out, aerr := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行改默认值: %v %+v", aerr, out)
			}
			// 撤回到「没有默认值」也要能走通（DROP DEFAULT 分支）。
			plan2, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("it_def"), Action: service.DDLSetDefault,
				Column: service.ColumnSpec{Name: "n"},
			})
			if err != nil {
				t.Fatalf("生成 DROP DEFAULT 预案: %v", err)
			}
			if !strings.Contains(plan2.Statements[0].SQL, "DROP DEFAULT") {
				t.Errorf("应为 DROP DEFAULT：%s", plan2.Statements[0].SQL)
			}
			if out, aerr := ex.ApplyChangePlan(ctx, plan2, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行 DROP DEFAULT: %v %+v", aerr, out)
			}
		})
	}
}
