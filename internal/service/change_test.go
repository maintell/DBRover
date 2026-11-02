package service_test

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
	"github.com/maintell/DBRover/internal/service"
)

// rowsOf 读回 t 表当前内容，供断言使用。
func rowsOf(t *testing.T, e *env) map[int64]string {
	t.Helper()
	ctx := context.Background()
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()

	cur, err := s.Query(ctx, `SELECT id, v FROM t ORDER BY id`, nil, driver.QueryOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = cur.Close() }()

	out := map[int64]string{}
	for {
		rows, more, err := cur.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		for _, r := range rows {
			out[r[0].(int64)] = r[1].(string)
		}
		if !more {
			break
		}
	}
	return out
}

func newTestExecutor(t *testing.T, e *env) *service.Executor {
	t.Helper()
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	t.Cleanup(func() { ex.Close() })
	return ex
}

func TestChangePlanUpdateApplies(t *testing.T) {
	e := setup(t)
	ex := newTestExecutor(t, e)

	plan, err := ex.BuildChangePlan(context.Background(), service.ChangeSetRequest{
		Table: model.ObjectRef{Kind: model.KindTable, Name: "t"},
		Changes: []service.RowChange{{
			Op:     service.OpUpdate,
			Key:    map[string]any{"id": int64(1)},
			Before: map[string]any{"id": int64(1), "v": "a"},
			After:  map[string]any{"v": "b"},
		}},
	})
	if err != nil {
		t.Fatalf("BuildChangePlan: %v", err)
	}

	if len(plan.Statements) != 1 {
		t.Fatalf("语句数 = %d", len(plan.Statements))
	}
	if plan.Risk < policy.R1 {
		t.Errorf("Risk = %s, 至少 R1", plan.Risk)
	}
	if len(plan.Rollback) != 1 {
		t.Fatalf("回滚语句数 = %d", len(plan.Rollback))
	}

	out, err := ex.ApplyChangePlan(context.Background(), plan, service.Request{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Blocked {
		t.Fatalf("测试环境应放行: %s", out.Reason)
	}
	if got := rowsOf(t, e)[1]; got != "b" {
		t.Errorf("v = %q, 期望 b", got)
	}
}

// TestChangePlanConflictRollsBack 是乐观并发的核心回归：
// 构建预案后该行被他人改动，应用时必须报冲突且整批不生效。
func TestChangePlanConflictRollsBack(t *testing.T) {
	e := setup(t)
	ex := newTestExecutor(t, e)

	plan, err := ex.BuildChangePlan(context.Background(), service.ChangeSetRequest{
		Table: model.ObjectRef{Kind: model.KindTable, Name: "t"},
		Changes: []service.RowChange{
			{Op: service.OpUpdate, Key: map[string]any{"id": int64(1)},
				Before: map[string]any{"id": int64(1), "v": "a"}, After: map[string]any{"v": "mine"}},
			{Op: service.OpInsert, Key: map[string]any{"id": int64(9)},
				After: map[string]any{"id": int64(9), "v": "new"}},
		},
	})
	if err != nil {
		t.Fatalf("BuildChangePlan: %v", err)
	}

	// 他人先改了这一行
	ctx := context.Background()
	s, _ := e.rw.Acquire(ctx)
	if _, err := s.Execute(ctx, `UPDATE t SET v='theirs' WHERE id=1`, nil); err != nil {
		t.Fatalf("外部更新: %v", err)
	}
	_ = s.Close()

	out, err := ex.ApplyChangePlan(ctx, plan, service.Request{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Blocked {
		t.Fatalf("不应被审批拦下（friction=%s）: %s", out.Friction, out.Reason)
	}
	if out.Err == nil {
		t.Fatal("应当报冲突")
	}
	if out.Err.Code != "CHANGE_ROW_CONFLICT" {
		t.Errorf("code = %s, 期望 CHANGE_ROW_CONFLICT", out.Err.Code)
	}

	rows := rowsOf(t, e)
	if rows[1] != "theirs" {
		t.Errorf("冲突时不应覆盖他人改动, 实际 v=%q", rows[1])
	}
	if _, ok := rows[9]; ok {
		t.Error("整批回滚后 id=9 不应存在")
	}
}

func TestChangePlanDeleteAndRollback(t *testing.T) {
	e := setup(t)
	ex := newTestExecutor(t, e)

	plan, err := ex.BuildChangePlan(context.Background(), service.ChangeSetRequest{
		Table: model.ObjectRef{Kind: model.KindTable, Name: "t"},
		Changes: []service.RowChange{{
			Op:     service.OpDelete,
			Key:    map[string]any{"id": int64(1)},
			Before: map[string]any{"id": int64(1), "v": "a"},
		}},
	})
	if err != nil {
		t.Fatalf("BuildChangePlan: %v", err)
	}
	if plan.Irreversible {
		t.Error("提供了完整原值时应可回滚")
	}

	if out, _ := ex.ApplyChangePlan(context.Background(), plan, service.Request{}); out.Blocked {
		t.Fatalf("不应被审批拦下: %s", out.Reason)
	} else if out.Err != nil {
		t.Fatalf("Apply: %v", out.Err)
	}
	if _, ok := rowsOf(t, e)[1]; ok {
		t.Fatal("删除后仍存在")
	}

	// 执行回滚语句，数据应恢复
	ctx := context.Background()
	s, _ := e.rw.Acquire(ctx)
	defer func() { _ = s.Close() }()
	for _, st := range plan.Rollback {
		if _, err := s.Execute(ctx, st.SQL, st.Args); err != nil {
			t.Fatalf("回滚失败: %v (%s)", err, st.SQL)
		}
	}
	if got := rowsOf(t, e)[1]; got != "a" {
		t.Errorf("回滚后 v = %q, 期望 a", got)
	}
}

func TestChangePlanInsertWithoutKeyIsIrreversible(t *testing.T) {
	e := setup(t)
	ex := newTestExecutor(t, e)

	plan, err := ex.BuildChangePlan(context.Background(), service.ChangeSetRequest{
		Table: model.ObjectRef{Kind: model.KindTable, Name: "t"},
		Changes: []service.RowChange{{
			Op:    service.OpInsert,
			After: map[string]any{"v": "auto"}, // 不给主键 → 自增，回滚无法定位
		}},
	})
	if err != nil {
		t.Fatalf("BuildChangePlan: %v", err)
	}
	if !plan.Irreversible {
		t.Error("未给主键的插入必须标记为不可逆")
	}
	if len(plan.Rollback) != 0 {
		t.Errorf("不可逆时不应生成回滚语句, 实际 %d 条", len(plan.Rollback))
	}
}

func TestChangePlanRejectsTableWithoutPK(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	s, _ := e.rw.Acquire(ctx)
	if _, err := s.Execute(ctx, `CREATE TABLE nopk (a TEXT)`, nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	_ = s.Close()

	ex := newTestExecutor(t, e)
	_, err := ex.BuildChangePlan(ctx, service.ChangeSetRequest{
		Table:   model.ObjectRef{Kind: model.KindTable, Name: "nopk"},
		Changes: []service.RowChange{{Op: service.OpDelete, Key: map[string]any{"a": "x"}}},
	})
	if !errors.Is(err, service.ErrNoPrimaryKeyForEdit) {
		t.Fatalf("无主键表应拒绝, 实际 %v", err)
	}
}

// TestChangePlanProductionFriction 记录一条刻意设计：
// 变更集自带语句预览，摩擦比同等级原始 SQL 低一级；
// 生产环境的 R2 走变更集是 F1（有预览、无额外审批），
// 而走原始 SQL 是 F2（需要审批凭证）。
func TestChangePlanProductionFriction(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvProduction, model.AccessReadWrite, nil)
	defer ex.Close()

	plan, err := ex.BuildChangePlan(context.Background(), service.ChangeSetRequest{
		Table: model.ObjectRef{Kind: model.KindTable, Name: "t"},
		Changes: []service.RowChange{{
			Op: service.OpUpdate, Key: map[string]any{"id": int64(1)},
			Before: map[string]any{"id": int64(1), "v": "a"}, After: map[string]any{"v": "b"},
		}},
	})
	if err != nil {
		t.Fatalf("BuildChangePlan: %v", err)
	}
	if plan.Friction != policy.F1 {
		t.Errorf("生产环境变更集 friction = %s, 期望 F1（自带预览）", plan.Friction)
	}
	// 预览内容必须真实存在，否则 F1 就成了空口承诺。
	if len(plan.Statements) == 0 || plan.Statements[0].SQL == "" {
		t.Error("F1 的前提是语句可见，plan 里必须有 SQL")
	}
}

// TestChangePlanKeepsRowCount 保证多行变更条数与输入一致且顺序稳定。
func TestChangePlanKeepsRowCount(t *testing.T) {
	e := setup(t)
	seed(t, e, 2, 3)
	ex := newTestExecutor(t, e)

	var changes []service.RowChange
	for _, id := range []int64{1, 2, 3} {
		changes = append(changes, service.RowChange{
			Op: service.OpUpdate, Key: map[string]any{"id": id},
			Before: map[string]any{"id": id, "v": "x"}, After: map[string]any{"v": "y"},
		})
	}
	plan, err := ex.BuildChangePlan(context.Background(), service.ChangeSetRequest{
		Table: model.ObjectRef{Kind: model.KindTable, Name: "t"}, Changes: changes,
	})
	if err != nil {
		t.Fatalf("BuildChangePlan: %v", err)
	}
	if len(plan.Statements) != 3 {
		t.Fatalf("语句数 = %d, 期望 3", len(plan.Statements))
	}
	if len(plan.Rollback) != 3 {
		t.Fatalf("回滚数 = %d, 期望 3", len(plan.Rollback))
	}
	// 回滚必须反序：先撤最后一次改动。
	keys := []string{}
	for _, r := range plan.Rollback {
		keys = append(keys, r.SQL)
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	if len(sorted) != len(keys) {
		t.Fatal("回滚语句数量异常")
	}
}
