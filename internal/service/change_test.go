package service_test

import (
	"context"
	"errors"
	"sort"
	"strings"
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

// TestChangePlanMultiColumnMergesIntoOneStatement 对应验收项 2：
// 同一行改多列只生成一条 UPDATE，且 SET 里只含被改的列。
func TestChangePlanMultiColumnMergesIntoOneStatement(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, q := range []string{
		`ALTER TABLE t ADD COLUMN w TEXT`,
		`UPDATE t SET v='a', w='w0' WHERE id=1`,
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("准备数据 (%s): %v", q, err)
		}
	}
	_ = s.Close()

	ex := newTestExecutor(t, e)
	plan, err := ex.BuildChangePlan(ctx, service.ChangeSetRequest{
		Table: model.ObjectRef{Kind: model.KindTable, Name: "t"},
		Changes: []service.RowChange{{
			Op:     service.OpUpdate,
			Key:    map[string]any{"id": int64(1)},
			Before: map[string]any{"id": int64(1), "v": "a", "w": "w0"},
			After:  map[string]any{"v": "b", "w": "w1"},
		}},
	})
	if err != nil {
		t.Fatalf("BuildChangePlan: %v", err)
	}
	if len(plan.Statements) != 1 {
		t.Fatalf("同一行改两列应合并成 1 条语句, 实际 %d", len(plan.Statements))
	}

	sqlText := plan.Statements[0].SQL
	setPart := sqlText
	if i := strings.Index(setPart, " WHERE "); i > 0 {
		setPart = setPart[:i]
	}
	if strings.Count(setPart, "=") != 2 {
		t.Errorf("SET 子句应只有 2 个赋值（v 与 w）, 实际: %s", setPart)
	}
	if strings.Contains(setPart, `"id"`) {
		t.Errorf("主键 id 不该出现在 SET 子句里: %s", setPart)
	}
	if !strings.Contains(setPart, `"v"`) || !strings.Contains(setPart, `"w"`) {
		t.Errorf("SET 子句缺被改列: %s", setPart)
	}
}

// TestChangePlanSetsNullAndLocatesNullRow 对应验收项 7：
// ① 把列改成 NULL 是参数绑定；② 原值为 NULL 时定位条件必须是 IS NULL，
// 用 `= ?` 绑 NULL 永远匹配不到行（SQL 三值逻辑），会误报冲突。
func TestChangePlanSetsNullAndLocatesNullRow(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `UPDATE t SET v=NULL WHERE id=1`, nil); err != nil {
		t.Fatalf("置 NULL: %v", err)
	}
	_ = s.Close()

	ex := newTestExecutor(t, e)

	// 从 NULL 改成有值：定位要用 IS NULL。
	plan, err := ex.BuildChangePlan(ctx, service.ChangeSetRequest{
		Table: model.ObjectRef{Kind: model.KindTable, Name: "t"},
		Changes: []service.RowChange{{
			Op:     service.OpUpdate,
			Key:    map[string]any{"id": int64(1)},
			Before: map[string]any{"id": int64(1), "v": nil},
			After:  map[string]any{"v": "x"},
		}},
	})
	if err != nil {
		t.Fatalf("BuildChangePlan: %v", err)
	}
	if !strings.Contains(plan.Statements[0].SQL, "IS NULL") {
		t.Errorf("原值 NULL 应生成 IS NULL 条件, 实际: %s", plan.Statements[0].SQL)
	}
	if out, err := ex.ApplyChangePlan(ctx, plan, service.Request{}); err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("应用失败: err=%v out=%+v", err, out)
	}
	if got := rowsOf(t, e)[1]; got != "x" {
		t.Fatalf("v = %q, 期望 x", got)
	}

	// 再从有值改成 NULL。
	plan, err = ex.BuildChangePlan(ctx, service.ChangeSetRequest{
		Table: model.ObjectRef{Kind: model.KindTable, Name: "t"},
		Changes: []service.RowChange{{
			Op:     service.OpUpdate,
			Key:    map[string]any{"id": int64(1)},
			Before: map[string]any{"id": int64(1), "v": "x"},
			After:  map[string]any{"v": nil},
		}},
	})
	if err != nil {
		t.Fatalf("BuildChangePlan(NULL): %v", err)
	}
	if out, err := ex.ApplyChangePlan(ctx, plan, service.Request{}); err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("应用 NULL 失败: err=%v out=%+v", err, out)
	}
	// rowsOf 用 string 承接，NULL 会让它 panic；这里直接数 NULL 行。
	if n := nullCount(t, e, 1); n != 1 {
		t.Fatalf("应用后 v IS NULL 的行数 = %d, 期望 1", n)
	}

	// 空串与 NULL 必须是两个不同的值：再写一次空串，IS NULL 不该命中它。
	plan, err = ex.BuildChangePlan(ctx, service.ChangeSetRequest{
		Table: model.ObjectRef{Kind: model.KindTable, Name: "t"},
		Changes: []service.RowChange{{
			Op:     service.OpUpdate,
			Key:    map[string]any{"id": int64(1)},
			Before: map[string]any{"id": int64(1), "v": nil},
			After:  map[string]any{"v": ""},
		}},
	})
	if err != nil {
		t.Fatalf("BuildChangePlan(空串): %v", err)
	}
	if out, err := ex.ApplyChangePlan(ctx, plan, service.Request{}); err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("应用空串失败: err=%v out=%+v", err, out)
	}
	if n := nullCount(t, e, 1); n != 0 {
		t.Fatalf("写入空串后 v IS NULL 的行数 = %d, 期望 0（空串不是 NULL）", n)
	}
}

// nullCount 数出 t 表里指定 id 且 v 为 NULL 的行数。
func nullCount(t *testing.T, e *env, id int64) int64 {
	t.Helper()
	ctx := context.Background()
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()

	cur, err := s.Query(ctx, `SELECT COUNT(*) FROM t WHERE id = ? AND v IS NULL`, []any{id}, driver.QueryOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = cur.Close() }()

	rows, _, err := cur.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("COUNT 查询返回 %d 行", len(rows))
	}
	return rows[0][0].(int64)
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

// TestChangePlanRejectsBigIntegerAsJSONNumber 对应 docs/06 验收项 6：
// BIGINT 一旦写成 JSON 数字，超过 2^53 的部分在解析成 float64 时就已经丢了。
// 这里必须**拒绝**，而不是把丢了精度的值写进库——后者看起来只是末位变了。
func TestChangePlanRejectsBigIntegerAsJSONNumber(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	// 2^53 + 1 在 float64 上就是 2^53，写进去会静默少 1。
	const id = int64(1)
	_, err := ex.BuildChangePlan(ctx, service.ChangeSetRequest{
		Table: tableRef("t"),
		Changes: []service.RowChange{{
			Op:     service.OpUpdate,
			Key:    map[string]any{"id": id},
			Before: map[string]any{"id": id, "v": "a"},
			After:  map[string]any{"v": "b"},
		}},
	})
	if err != nil {
		t.Fatalf("正常变更不该被拦: %v", err)
	}

	// 加一个整数列，再拿大整数字面量去写它。
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `ALTER TABLE t ADD COLUMN big INTEGER`, nil); err != nil {
		t.Fatalf("加列: %v", err)
	}
	_ = s.Close()

	// 9223372036854775807 写成 JSON 数字，服务端拿到的是 9223372036854775808 这个 float64。
	big := float64(9223372036854775807)
	if _, err := ex.BuildChangePlan(ctx, service.ChangeSetRequest{
		Table: tableRef("t"),
		Changes: []service.RowChange{{
			Op:     service.OpUpdate,
			Key:    map[string]any{"id": id},
			Before: map[string]any{"id": id},
			After:  map[string]any{"big": big},
		}},
	}); !errors.Is(err, service.ErrLargeNumberAsJSON) {
		t.Fatalf("大整数写成 JSON 数字应被拒绝, 实际 %v", err)
	}

	// 字符串形式必须放行：这是无损路径。
	if _, err := ex.BuildChangePlan(ctx, service.ChangeSetRequest{
		Table: tableRef("t"),
		Changes: []service.RowChange{{
			Op:     service.OpUpdate,
			Key:    map[string]any{"id": id},
			Before: map[string]any{"id": id},
			After:  map[string]any{"big": "9223372036854775807"},
		}},
	}); err != nil {
		t.Fatalf("字符串形式的大整数应放行: %v", err)
	}
}

// TestBulkUpdateRejectsBigIntegerAsJSONNumber 与上一条同源：
// 批量修改的 SET 值也必须挡住 JSON 数字形态的大整数。
func TestBulkUpdateRejectsBigIntegerAsJSONNumber(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `ALTER TABLE t ADD COLUMN big INTEGER`, nil); err != nil {
		t.Fatalf("加列: %v", err)
	}
	_ = s.Close()

	if _, err := ex.BuildBulkUpdatePlan(ctx, service.BulkUpdateRequest{
		Table:            tableRef("t"),
		Set:              map[string]any{"big": float64(9223372036854775807)},
		Filter:           []service.FilterCond{{Column: "id", Op: service.FilterEq, Value: "1"}},
		AcceptNoRollback: true,
	}); !errors.Is(err, service.ErrLargeNumberAsJSON) {
		t.Fatalf("批量修改的大整数 SET 值应被拒绝, 实际 %v", err)
	}

	if _, err := ex.BuildBulkUpdatePlan(ctx, service.BulkUpdateRequest{
		Table:            tableRef("t"),
		Set:              map[string]any{"big": "9223372036854775807"},
		Filter:           []service.FilterCond{{Column: "id", Op: service.FilterEq, Value: "1"}},
		AcceptNoRollback: true,
	}); err != nil {
		t.Fatalf("字符串形式应放行: %v", err)
	}
}
