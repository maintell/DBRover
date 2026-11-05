package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestChangePlanInsertThreeStates 钉住「不写 / NULL / 空串」三种状态。
//
// 这三件事在数据库里完全不同：不写给的是默认值或自增，NULL 是空值，空串是一个真实
// 的零长度字符串。服务端靠 `After` 里「有没有这个键」与「值是不是 nil」来区分，
// 界面靠每列一个三态选择器表达。这条用例从**语句内容**与**回读结果**两头锁住它：
// 只看回读不够——把「不写」误写成 NULL 时，有默认值的列会变成 NULL 而不是 7。
func TestChangePlanInsertThreeStates(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `ALTER TABLE t ADD COLUMN n INTEGER DEFAULT 7`, nil); err != nil {
		t.Fatalf("加列: %v", err)
	}
	_ = s.Close()

	ex := newTestExecutor(t, e)
	plan, err := ex.BuildChangePlan(ctx, service.ChangeSetRequest{
		Table: model.ObjectRef{Kind: model.KindTable, Name: "t"},
		Changes: []service.RowChange{
			// 10：显式 NULL。
			{Op: service.OpInsert, Key: map[string]any{"id": int64(10)},
				After: map[string]any{"id": int64(10), "v": nil}},
			// 11：空串——不是 NULL。
			{Op: service.OpInsert, Key: map[string]any{"id": int64(11)},
				After: map[string]any{"id": int64(11), "v": ""}},
			// 12：n 根本不出现，走默认值 7。
			{Op: service.OpInsert, Key: map[string]any{"id": int64(12)},
				After: map[string]any{"id": int64(12), "v": "x"}},
		},
	})
	if err != nil {
		t.Fatalf("BuildChangePlan: %v", err)
	}
	if len(plan.Statements) != 3 {
		t.Fatalf("语句数 = %d, 期望 3", len(plan.Statements))
	}

	// 语句内容：「写 NULL」必须带上 v 列；「不写 n」的语句里不能出现 n。
	if !strings.Contains(plan.Statements[0].SQL, `"v"`) {
		t.Errorf("显式 NULL 的插入没带 v 列：%s", plan.Statements[0].SQL)
	}
	if len(plan.Statements[0].Args) != 2 || plan.Statements[0].Args[1] != nil {
		t.Errorf("显式 NULL 的参数应为 [10 nil]，实际 %#v", plan.Statements[0].Args)
	}
	if !strings.Contains(plan.Statements[1].SQL, `"v"`) {
		t.Errorf("空串的插入没带 v 列：%s", plan.Statements[1].SQL)
	}
	if v, ok := plan.Statements[1].Args[1].(string); !ok || v != "" {
		t.Errorf("空串的参数应为空字符串，实际 %#v", plan.Statements[1].Args)
	}
	if strings.Contains(plan.Statements[2].SQL, `"n"`) {
		t.Errorf("没给值的列不该出现在 INSERT 里：%s", plan.Statements[2].SQL)
	}

	out, err := ex.ApplyChangePlan(ctx, plan, service.Request{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Blocked {
		t.Fatalf("测试环境应放行: %s", out.Reason)
	}
	if out.Err != nil {
		t.Fatalf("执行失败: %v", out.Err)
	}

	// 回读：id=10 是 NULL，id=11 不是 NULL（是空串），id=12 的 n 是默认值 7。
	if got := scalarOf(t, e, `SELECT v IS NULL FROM t WHERE id = 10`); got != int64(1) {
		t.Errorf("id=10 应是 NULL，v IS NULL = %v", got)
	}
	if got := scalarOf(t, e, `SELECT v IS NULL FROM t WHERE id = 11`); got != int64(0) {
		t.Errorf("id=11 应是空串而不是 NULL，v IS NULL = %v", got)
	}
	if got := scalarOf(t, e, `SELECT n FROM t WHERE id = 12`); got != int64(7) {
		t.Errorf("id=12 的 n 应回落默认值 7，实际 %v", got)
	}
}

// scalarOf 跑一条只返回一个值的查询。
func scalarOf(t *testing.T, e *env, q string) any {
	t.Helper()
	ctx := context.Background()
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()

	cur, err := s.Query(ctx, q, nil, driver.QueryOptions{})
	if err != nil {
		t.Fatalf("Query (%s): %v", q, err)
	}
	defer func() { _ = cur.Close() }()

	rows, _, err := cur.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: 期望 1 行 1 列，实际 %d 行", q, len(rows))
	}
	return rows[0][0]
}
