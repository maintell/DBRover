package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestDDLCreateSequenceRejectedEngines 钉住「没有序列对象就别说能建」。
//
// MySQL 用 AUTO_INCREMENT、SQLite 用 AUTOINCREMENT，都没有独立序列对象；
// 生成 CREATE SEQUENCE 只会得到一条必然失败的语句。
func TestDDLCreateSequenceRejectedEngines(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	_, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table: tableRef("t"), Action: service.DDLCreateSequence,
		Sequence: service.SequenceSpec{Name: "seq_a"},
	})
	if !errors.Is(err, service.ErrInvalidDDL) {
		t.Fatalf("SQLite 上建序列应被拒, 实际 %v", err)
	}
	if !strings.Contains(err.Error(), "AUTOINCREMENT") {
		t.Errorf("要说明这个引擎用别的方式做自增：%v", err)
	}
}

// TestDDLCreateSequenceRejectsBadInput 覆盖参数校验（与引擎无关的部分）。
//
// 这些错在哪台引擎上都不成立，所以必须比「引擎不支持」先报出来。
func TestDDLCreateSequenceRejectsBadInput(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	zero := int64(0)
	big := int64(100)
	small := int64(10)
	five := int64(5)
	cases := []struct {
		name string
		spec service.SequenceSpec
		want string
	}{
		{"名字非法", service.SequenceSpec{Name: "se q"}, "字母、数字、下划线"},
		{"步长为 0", service.SequenceSpec{Name: "seq_a", Increment: &zero}, "步长不能为 0"},
		{"最小大于最大", service.SequenceSpec{Name: "seq_a", Min: &big, Max: &small}, "最小值 100 大于最大值 10"},
		{"起始值越界", service.SequenceSpec{Name: "seq_a", Min: &small, Max: &big, Start: &five}, "小于最小值"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("t"), Action: service.DDLCreateSequence, Sequence: c.spec,
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

// TestDDLCreateSequenceOnRealPG 验证建序列：语句带参数、nextval 按参数走、回滚能删。
func TestDDLCreateSequenceOnRealPG(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		if tgt.name != "pg" {
			// 不能在这里 t.Skip：MySQL 排在前，跳过会终止整条用例，
			// pg 那一段根本没跑过（这一条此前因此一直是「假绿」）。
			continue
		}
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{"DROP SEQUENCE IF EXISTS it_seq"}, nil)

			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			inc := int64(5)
			start := int64(100)
			plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("it_seq"), Action: service.DDLCreateSequence,
				Sequence: service.SequenceSpec{Name: "it_seq", Start: &start, Increment: &inc},
			})
			if err != nil {
				t.Fatalf("生成预案: %v", err)
			}
			if !strings.Contains(plan.Statements[0].SQL, "START WITH 100") ||
				!strings.Contains(plan.Statements[0].SQL, "INCREMENT BY 5") {
				t.Fatalf("语句丢了参数：%s", plan.Statements[0].SQL)
			}
			if out, aerr := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行建序列: %v %+v", aerr, out)
			}
			if err := execStrict(ctx, ex, "SELECT nextval('it_seq')"); err != nil {
				t.Fatalf("nextval: %v", err)
			}
			for _, s := range plan.Rollback {
				if err := execStrict(ctx, ex, s.SQL); err != nil {
					t.Fatalf("回滚（%s）: %v", s.SQL, err)
				}
			}
			if err := execStrict(ctx, ex, "SELECT nextval('it_seq')"); err == nil {
				t.Fatal("回滚后序列应当不存在")
			}
		})
	}
}
