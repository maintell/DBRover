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

// TestDDLAddCheckRejectedEngines 钉住「宁可拒绝，也不做假成功」。
//
// MySQL 5.7 会解析 CHECK 但静默忽略；SQLite 只能在建表时写 CHECK。这两种情况
// 下生成语句都会得到「建完了、约束不生效」——比报错危险得多。
func TestDDLAddCheckRejectedEngines(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	_, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table: tableRef("t"), Action: service.DDLAddCheck,
		Check: service.CheckSpec{Name: "ck_v", Expression: "v IS NOT NULL"},
	})
	if !errors.Is(err, service.ErrInvalidDDL) {
		t.Fatalf("SQLite 上加检查约束应被拒, 实际 %v", err)
	}
	if !strings.Contains(err.Error(), "建表时") {
		t.Errorf("要说明 SQLite 只能在建表时写 CHECK：%v", err)
	}
}

// TestDDLAddCheckRejectsBadInput 覆盖表达式与约束名的校验（与引擎无关）。
func TestDDLAddCheckRejectsBadInput(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	cases := []struct{ name, ckName, expr, want string }{
		{"空表达式", "ck_a", "  ", "不能为空"},
		{"带分号", "ck_a", "v IS NOT NULL; DROP TABLE t", "分号"},
		{"带行注释", "ck_a", "v IS NOT NULL -- x", "注释符"},
		{"带块注释", "ck_a", "v /* x */ IS NOT NULL", "注释符"},
		{"约束名带空格", "ck a", "v IS NOT NULL", "字母、数字、下划线"},
		{"约束名以数字开头", "1ck", "v IS NOT NULL", "字母、数字、下划线"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("t"), Action: service.DDLAddCheck,
				Check: service.CheckSpec{Name: c.ckName, Expression: c.expr},
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

// execStrict 执行一条语句，并把三种失败合成一个 error。
//
// 为什么要它：执行门把「被策略拦下」和「执行期错误」都放在 Outcome 里，返回的
// error 只是「根本没跑起来」。只判 error == nil 会把失败当成功——这条约束用例
// 第一版就是这么写的，结果 PG 上「插违规行成功」时它仍然通过。
func execStrict(ctx context.Context, ex *service.Executor, sql string) error {
	out, err := ex.Execute(ctx, service.Request{SQL: sql, Approval: true})
	if err != nil {
		return err
	}
	if out == nil {
		return errors.New("执行结果为空")
	}
	if out.Blocked {
		return errors.New("被拦下：" + out.Reason)
	}
	if out.Err != nil {
		return errors.New(out.Err.Message)
	}
	return nil
}

// TestDDLAddCheckOnRealEngines 在真实引擎上验证「加检查约束」。
//
// MySQL 分支刻意先证明「5.7 会静默忽略 CHECK」，再断言我们拒绝：这条注释在
// 代码里写着，就得有真机证据，否则下一个人会把它当成主观判断删掉。
func TestDDLAddCheckOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP TABLE IF EXISTS it_check",
				"CREATE TABLE it_check (id INTEGER PRIMARY KEY, age INTEGER)",
			}, func(ctx context.Context, s driver.Session) {
				_, _ = s.Execute(ctx, "INSERT INTO it_check (id, age) VALUES (1, 5)", nil)
				_, _ = s.Execute(ctx, "INSERT INTO it_check (id, age) VALUES (2, -1)", nil)
			})

			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			req := service.DDLRequest{
				Table: tableRef("it_check"), Action: service.DDLAddCheck,
				Check: service.CheckSpec{Name: "ck_age", Expression: "age >= 0"},
			}

			if tgt.name == "mysql" {
				// ① 先证明 MySQL 5.7 接受 CHECK 却不强制。
				if err := execStrict(ctx, ex, "ALTER TABLE it_check ADD CONSTRAINT ck_probe CHECK (age >= 0)"); err != nil {
					t.Fatalf("MySQL 5.7 本应接受 CHECK 语法（才谈得上静默忽略）：%v", err)
				}
				if err := execStrict(ctx, ex, "INSERT INTO it_check (id, age) VALUES (3, -100)"); err != nil {
					t.Fatalf("MySQL 5.7 本应忽略 CHECK，插入却被拒——那它就该算支持：%v", err)
				}
				// ② 所以我们拒绝，并说清版本边界。
				_, err := ex.BuildDDLPlan(ctx, req)
				if !errors.Is(err, service.ErrInvalidDDL) {
					t.Fatalf("MySQL 上应拒绝加检查约束, 实际 %v", err)
				}
				if !strings.Contains(err.Error(), "8.0.16") {
					t.Errorf("要写清从哪个版本起才强制：%v", err)
				}
				return
			}

			// PostgreSQL：有违规行时必须先拦住并报行数。
			if _, err := ex.BuildDDLPlan(ctx, req); !errors.Is(err, service.ErrCheckViolated) {
				t.Fatalf("有违规行应返回 ErrCheckViolated, 实际 %v", err)
			} else if !strings.Contains(err.Error(), "1 行") {
				t.Errorf("要报出多少行不满足：%v", err)
			}

			if err := execStrict(ctx, ex, "UPDATE it_check SET age = 0 WHERE id = 2"); err != nil {
				t.Fatalf("修数据: %v", err)
			}
			plan, err := ex.BuildDDLPlan(ctx, req)
			if err != nil {
				t.Fatalf("生成预案: %v", err)
			}
			if plan.AffectedRows != 0 {
				t.Errorf("加约束不改数据，影响行数应为 0，实际 %d", plan.AffectedRows)
			}
			if out, aerr := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行加约束: %v %+v", aerr, out)
			}
			// 约束真的生效：插违规行必须被拒。
			//
			// 注意用 execStrict：执行门的「执行期错误」放在 out.Err 里，返回的
			// error 是 nil。只看 error 会把失败当成成功——这个坑值得记一笔。
			if err := execStrict(ctx, ex, "INSERT INTO it_check (id, age) VALUES (9, -5)"); err == nil {
				t.Fatal("约束应当拒绝违规行")
			}
			// 回滚能摘掉它。
			for _, s := range plan.Rollback {
				if err := execStrict(ctx, ex, s.SQL); err != nil {
					t.Fatalf("回滚（%s）: %v", s.SQL, err)
				}
			}
			if err := execStrict(ctx, ex, "INSERT INTO it_check (id, age) VALUES (10, -5)"); err != nil {
				t.Fatalf("回滚后应能插入违规行（说明约束真被摘掉了）：%v", err)
			}
		})
	}
}
