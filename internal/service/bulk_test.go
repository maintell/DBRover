package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/service"
)

// TestBulkUpdateRequiresTableNameWhenNoFilter 对应 docs/06 §3.2：
// 无条件批量修改等于全表写，必须回填表名才允许生成预案。
func TestBulkUpdateRequiresTableNameWhenNoFilter(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)
	seed(t, e, 2, 3)

	req := service.BulkUpdateRequest{
		Table: tableRef("t"),
		Set:   map[string]any{"v": "bulk"},
	}
	if _, err := ex.BuildBulkUpdatePlan(ctx, req); !errors.Is(err, service.ErrBulkNeedsConfirmTable) {
		t.Fatalf("无条件批量修改应要求回填表名, 实际 %v", err)
	}

	// 回填正确的表名后放行，且风险应升到 R4（全表写）。
	req.ConfirmTable = "t"
	res, err := ex.BuildBulkUpdatePlan(ctx, req)
	if err != nil {
		t.Fatalf("回填表名后应放行: %v", err)
	}
	if res.Affected != 4 {
		t.Errorf("影响行数 = %d, 期望 4", res.Affected)
	}
	if res.Plan.Risk.String() != "R4" {
		t.Errorf("全表写风险 = %s, 期望 R4", res.Plan.Risk)
	}

	// 表名写错仍然拒绝。
	req.ConfirmTable = "other"
	if _, err := ex.BuildBulkUpdatePlan(ctx, req); !errors.Is(err, service.ErrBulkNeedsConfirmTable) {
		t.Errorf("表名不匹配应拒绝, 实际 %v", err)
	}
}

// TestBulkUpdateWithFilterGeneratesUndo 验证按条件改少量行时：
// 影响行数精确、回滚逐行、样本给出前后对比。
func TestBulkUpdateWithFilterGeneratesUndo(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)
	seed(t, e, 2, 5) // id 1..6

	res, err := ex.BuildBulkUpdatePlan(ctx, service.BulkUpdateRequest{
		Table:  tableRef("t"),
		Set:    map[string]any{"v": "new"},
		Filter: []service.FilterCond{{Column: "id", Op: service.FilterLe, Value: "3"}},
	})
	if err != nil {
		t.Fatalf("BuildBulkUpdatePlan: %v", err)
	}
	if res.Affected != 3 {
		t.Fatalf("影响行数 = %d, 期望 3", res.Affected)
	}
	if res.Plan.Irreversible {
		t.Error("少量行应生成回滚，不该标不可逆")
	}
	if len(res.Plan.Rollback) != 3 {
		t.Errorf("回滚语句数 = %d, 期望 3", len(res.Plan.Rollback))
	}
	if len(res.Samples) != 3 {
		t.Errorf("样本数 = %d, 期望 3", len(res.Samples))
	}
	if res.Samples[0].Before["v"] != "a" || res.Samples[0].After["v"] != "new" {
		t.Errorf("前后对比不对: %+v", res.Samples[0])
	}
	if res.Plan.Statements[0].ExpectRows != 3 {
		t.Errorf("expect_rows = %d, 期望 3", res.Plan.Statements[0].ExpectRows)
	}

	out, err := ex.ApplyChangePlan(ctx, res.Plan, service.Request{Approval: true})
	if err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("应用批量修改失败: err=%v out=%+v", err, out)
	}
	if got := rowsOf(t, e)[1]; got != "new" {
		t.Errorf("id=1 的 v = %q, 期望 new", got)
	}
	if got := rowsOf(t, e)[6]; got != "x" {
		t.Errorf("id=6 不该被改, 实际 %q", got)
	}

	// 回滚预案能把值改回去（id=1 的原值是 'a'，不是 'x'）。
	if out, err := ex.ApplyChangePlan(ctx, &service.ChangePlan{
		ID:         "plan-undo-bulk",
		Table:      tableRef("t"),
		Statements: res.Plan.Rollback,
		Risk:       res.Plan.Risk,
		Friction:   res.Plan.Friction,
		ExpiresAt:  res.Plan.ExpiresAt,
	}, service.Request{Approval: true}); err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("回滚失败: err=%v out=%+v", err, out)
	}
	if got := rowsOf(t, e)[1]; got != "a" {
		t.Errorf("回滚后 id=1 的 v = %q, 期望 a", got)
	}
}

// TestBulkUpdateConflictRollsBack 验证并发：统计之后有人插入了同样命中条件的行，
// 执行时行数对不上，整批必须回滚而不是改多行。
func TestBulkUpdateConflictRollsBack(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)
	seed(t, e, 2, 3) // id=1 的 v 是 'a'，id=2..4 是 'x'

	res, err := ex.BuildBulkUpdatePlan(ctx, service.BulkUpdateRequest{
		Table:  tableRef("t"),
		Set:    map[string]any{"v": "conflict"},
		Filter: []service.FilterCond{{Column: "v", Op: service.FilterEq, Value: "x"}},
	})
	if err != nil {
		t.Fatalf("BuildBulkUpdatePlan: %v", err)
	}
	if res.Affected != 3 {
		t.Fatalf("影响行数 = %d, 期望 3", res.Affected)
	}

	// 模拟别人又插了一行，而且同样命中 v='x'。
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `INSERT INTO t (id, v) VALUES (5, 'x')`, nil); err != nil {
		t.Fatalf("插入: %v", err)
	}
	_ = s.Close()

	out, err := ex.ApplyChangePlan(ctx, res.Plan, service.Request{Approval: true})
	if err != nil {
		t.Fatalf("ApplyChangePlan: %v", err)
	}
	if out.Err == nil || out.Err.Code != "CHANGE_ROW_CONFLICT" {
		t.Fatalf("应报行数冲突, 实际 out=%+v", out)
	}
	for id, v := range rowsOf(t, e) {
		if v == "conflict" {
			t.Fatalf("冲突时不该有行被改（id=%d）", id)
		}
	}
}

// TestBulkUpdateRejectsPrimaryKeyAndUnknownColumn 覆盖参数校验。
func TestBulkUpdateRejectsPrimaryKeyAndUnknownColumn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	cases := []struct {
		name string
		req  service.BulkUpdateRequest
	}{
		{"空 set", service.BulkUpdateRequest{Table: tableRef("t"), Set: map[string]any{}}},
		{"改主键", service.BulkUpdateRequest{Table: tableRef("t"), Set: map[string]any{"id": 9}, ConfirmTable: "t"}},
		{"列不存在", service.BulkUpdateRequest{Table: tableRef("t"), Set: map[string]any{"nope": 1}, ConfirmTable: "t"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ex.BuildBulkUpdatePlan(ctx, c.req); !errors.Is(err, service.ErrInvalidDDL) {
				t.Fatalf("应返回 ErrInvalidDDL, 实际 %v", err)
			}
		})
	}
}

// TestBulkUpdateLargeRequiresAck 验证影响行数超过上限时的行为：
// 不生成回滚、标不可逆、且必须显式接受。
func TestBulkUpdateLargeRequiresAck(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	// 造 1001 行（超过 bulkUndoLimit）。用一条递归 CTE 插入，
	// 逐行插会跑上百秒——那是 SQLite 每次事务 fsync 的代价，不是被测逻辑的问题。
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx,
		`INSERT INTO t (id, v)
		 WITH RECURSIVE seq(x) AS (SELECT 2 UNION ALL SELECT x+1 FROM seq WHERE x < 1001)
		 SELECT x, 'x' FROM seq`, nil); err != nil {
		t.Fatalf("批量插入: %v", err)
	}
	_ = s.Close()

	req := service.BulkUpdateRequest{
		Table:        tableRef("t"),
		Set:          map[string]any{"v": "big"},
		ConfirmTable: "t",
	}
	if _, err := ex.BuildBulkUpdatePlan(ctx, req); !errors.Is(err, service.ErrBulkNeedsNoRollbackAck) {
		t.Fatalf("超过上限应要求显式接受, 实际 %v", err)
	}

	req.AcceptNoRollback = true
	res, err := ex.BuildBulkUpdatePlan(ctx, req)
	if err != nil {
		t.Fatalf("接受后应放行: %v", err)
	}
	if !res.Plan.Irreversible {
		t.Error("不生成回滚时应标不可逆")
	}
	if !strings.Contains(res.Note, "不生成回滚") {
		t.Errorf("应给出说明, 实际 %q", res.Note)
	}
}
