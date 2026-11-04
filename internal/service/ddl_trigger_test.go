package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestDDLCreateTriggerValidation 覆盖参数校验（在 SQLite 上跑，它支持建触发器）。
func TestDDLCreateTriggerValidation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	ok := service.DDLRequest{
		Table: tableRef("t"), Action: service.DDLCreateTrigger,
		Trigger: service.TriggerSpec{Name: "trg_ok", Timing: "AFTER", Event: "INSERT", Body: "UPDATE t SET v = v"},
	}
	if _, err := ex.BuildDDLPlan(ctx, ok); err != nil {
		t.Fatalf("基准用例应当能出预案：%v", err)
	}
	// 先建一个，用来验重名。
	plan, err := ex.BuildDDLPlan(ctx, ok)
	if err != nil {
		t.Fatal(err)
	}
	if out, aerr := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
		t.Fatalf("建触发器: %v %+v", aerr, out)
	}

	bad := []struct {
		name string
		spec service.TriggerSpec
		want string
	}{
		{"名字非法", service.TriggerSpec{Name: "tr g", Timing: "AFTER", Event: "INSERT", Body: "UPDATE t SET v = v"}, "字母、数字、下划线"},
		{"时机非法", service.TriggerSpec{Name: "trg_a", Timing: "DURING", Event: "INSERT", Body: "UPDATE t SET v = v"}, "时机只能是"},
		{"事件非法", service.TriggerSpec{Name: "trg_a", Timing: "AFTER", Event: "TRUNCATE", Body: "UPDATE t SET v = v"}, "事件只能是"},
		{"体为空", service.TriggerSpec{Name: "trg_a", Timing: "AFTER", Event: "INSERT"}, "体不能为空"},
		{"体带分号", service.TriggerSpec{Name: "trg_a", Timing: "AFTER", Event: "INSERT", Body: "UPDATE t SET v = v; DELETE FROM t"}, "只支持单条语句"},
		{"重名", service.TriggerSpec{Name: "trg_ok", Timing: "AFTER", Event: "INSERT", Body: "UPDATE t SET v = v"}, "已存在"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("t"), Action: service.DDLCreateTrigger, Trigger: c.spec,
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

// TestDDLCreateTriggerWorksOnSQLite 验证 SQLite 的件真的生效：AFTER INSERT 把 n 翻倍。
//
// SQLite 的触发器件是 BEGIN … END，里面有分号——顺手也验证切分器没有把它切断。
func TestDDLCreateTriggerWorksOnSQLite(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	for _, q := range []string{
		"DROP TRIGGER IF EXISTS trg_double",
		"DROP TABLE IF EXISTS trg_t",
		"CREATE TABLE trg_t (id INTEGER PRIMARY KEY, n INTEGER)",
	} {
		if err := execStrict(ctx, ex, q); err != nil {
			t.Fatalf("准备（%s）: %v", q, err)
		}
	}
	t.Cleanup(func() {
		_ = execStrict(context.Background(), ex, "DROP TRIGGER IF EXISTS trg_double")
		_ = execStrict(context.Background(), ex, "DROP TABLE IF EXISTS trg_t")
	})

	plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table: tableRef("trg_t"), Action: service.DDLCreateTrigger,
		Trigger: service.TriggerSpec{Name: "trg_double", Timing: "AFTER", Event: "INSERT",
			Body: "UPDATE trg_t SET n = n * 2 WHERE id = NEW.id"},
	})
	if err != nil {
		t.Fatalf("生成预案: %v", err)
	}
	if out, aerr := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
		t.Fatalf("执行建触发器: %v %+v", aerr, out)
	}
	if err := execStrict(ctx, ex, "INSERT INTO trg_t (id, n) VALUES (1, 5)"); err != nil {
		t.Fatalf("插数据: %v", err)
	}
	if n := countRows(t, ctx, ex, "SELECT n FROM trg_t WHERE id = 1"); n != 10 {
		t.Fatalf("触发器没生效：n = %d，期望 10", n)
	}
	// 回滚删掉之后不再翻倍。
	for _, s := range plan.Rollback {
		if err := execStrict(ctx, ex, s.SQL); err != nil {
			t.Fatalf("回滚（%s）: %v", s.SQL, err)
		}
	}
	if err := execStrict(ctx, ex, "INSERT INTO trg_t (id, n) VALUES (2, 5)"); err != nil {
		t.Fatalf("回滚后插数据: %v", err)
	}
	if n := countRows(t, ctx, ex, "SELECT n FROM trg_t WHERE id = 2"); n != 5 {
		t.Fatalf("回滚后不该再翻倍：n = %d，期望 5", n)
	}
}

// TestDDLCreateTriggerOnRealEngines：MySQL 上建 BEFORE INSERT 把值转大写；
// PG 上必须被拒并说明要先建函数。
func TestDDLCreateTriggerOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			// 不单独 DROP TRIGGER：MySQL 删表会连触发器一起删，而 PostgreSQL 的
			// DROP TRIGGER 必须带 ON <表> 且不支持 IF EXISTS——写进共用清单会在
			// PG 上给出一个与产品无关的语法错误。
			tgt.prepareTable(t, []string{"DROP TABLE IF EXISTS it_trgt"}, nil)
			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()
			if err := execStrict(ctx, ex, "CREATE TABLE it_trgt (id INTEGER PRIMARY KEY, v VARCHAR(20))"); err != nil {
				t.Fatalf("建表: %v", err)
			}
			t.Cleanup(func() {
				_ = execStrict(context.Background(), ex, "DROP TABLE IF EXISTS it_trgt")
			})

			req := service.DDLRequest{
				Table: tableRef("it_trgt"), Action: service.DDLCreateTrigger,
				Trigger: service.TriggerSpec{Name: "it_trg", Timing: "BEFORE", Event: "INSERT",
					Body: "SET NEW.v = UPPER(NEW.v)"},
			}
			plan, err := ex.BuildDDLPlan(ctx, req)
			if tgt.name == "pg" {
				if !errors.Is(err, service.ErrInvalidDDL) || !strings.Contains(err.Error(), "CREATE FUNCTION") {
					t.Fatalf("PG 上应拒绝并说明要先建函数, 实际 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("生成预案: %v", err)
			}
			if out, aerr := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行: %v %+v", aerr, out)
			}
			if err := execStrict(ctx, ex, "INSERT INTO it_trgt (id, v) VALUES (1, 'abc')"); err != nil {
				t.Fatalf("插数据: %v", err)
			}
			if got := rowText(t, ctx, ex, "SELECT v FROM it_trgt WHERE id = 1"); got != "ABC" {
				t.Fatalf("触发器没生效：v = %q，期望 ABC", got)
			}
		})
	}
}

// rowText 取第一行第一列的文本值。
func rowText(t *testing.T, ctx context.Context, ex *service.Executor, sql string) string {
	t.Helper()
	out, err := ex.Execute(ctx, service.Request{SQL: sql, Approval: true})
	if err != nil || out == nil || out.Err != nil || len(out.Results) != 1 || len(out.Results[0].Rows) != 1 {
		t.Fatalf("查询 %q 失败：%v %+v", sql, err, out)
	}
	v := out.Results[0].Rows[0][0]
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	t.Fatalf("结果不是文本：%T", v)
	return ""
}
