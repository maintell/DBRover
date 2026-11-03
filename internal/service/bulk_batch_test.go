package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/service"
)

// prepBig 建一张 big(id,v) 表并一次写入 n 行，全部 v='x'。
// 用递归 CTE 造数：逐行插入在 SQLite 上每行一个事务，2500 行要跑几分钟。
func prepBig(t *testing.T, e *env, n int) {
	t.Helper()
	ctx := context.Background()
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.Execute(ctx, `CREATE TABLE big (id INTEGER PRIMARY KEY, v TEXT)`, nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	if _, err := s.Execute(ctx, `
		INSERT INTO big (id, v)
		WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM seq WHERE x < ?)
		SELECT x, 'x' FROM seq`, []any{n}); err != nil {
		t.Fatalf("造数: %v", err)
	}
}

// countBig 返回 big 表里满足条件的行数。
func countBig(t *testing.T, e *env, where string, args ...any) int64 {
	t.Helper()
	ctx := context.Background()
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()

	cur, err := s.Query(ctx, `SELECT COUNT(*) FROM big WHERE `+where, args, driver.QueryOptions{})
	if err != nil {
		t.Fatalf("count (%s): %v", where, err)
	}
	defer func() { _ = cur.Close() }()
	rows, _, err := cur.Next(ctx)
	if err != nil || len(rows) == 0 {
		t.Fatalf("count (%s): %v", where, err)
	}
	n, ok := rows[0][0].(int64)
	if !ok {
		t.Fatalf("count (%s) 返回值不是 int64: %T", where, rows[0][0])
	}
	return n
}

// TestBulkBatchesSplitByKeyset 对应 docs/06 §3.2 与验收项 8：
// 影响行数超过单批上限时按主键分批推进，且不重不漏。
//
// 这里刻意不带筛选条件：如果推进不靠键集而是「改完再查一遍」，
// 每批都会重新命中 id 1..1000，原地打转到超时。所以这条用例的
// 超时本身就是判据之一。
func TestBulkBatchesSplitByKeyset(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ex := newTestExecutor(t, e)
	prepBig(t, e, 2500)

	res, err := ex.BuildBulkUpdatePlan(ctx, service.BulkUpdateRequest{
		Table:            tableRef("big"),
		Set:              map[string]any{"v": "done"},
		ConfirmTable:     "big",
		AcceptNoRollback: true,
	})
	if err != nil {
		t.Fatalf("BuildBulkUpdatePlan: %v", err)
	}
	if res.Affected != 2500 {
		t.Fatalf("影响行数 = %d, 期望 2500", res.Affected)
	}
	if !res.Batched {
		t.Fatal("2500 行应启用分批执行")
	}
	if res.BatchSize != service.DefaultBulkBatchSize {
		t.Errorf("batch_size = %d, 期望 %d", res.BatchSize, service.DefaultBulkBatchSize)
	}
	if res.Batches != 3 {
		t.Errorf("批次数 = %d, 期望 3", res.Batches)
	}
	if !strings.Contains(res.Note, "3 批") {
		t.Errorf("说明里应写明分批数, 实际 %q", res.Note)
	}

	var got []int64
	out, err := ex.ApplyChangePlanWithProgress(ctx, res.Plan, service.Request{Approval: true},
		func(done, total int64) { got = append(got, done) })
	if err != nil {
		t.Fatalf("ApplyChangePlanWithProgress: %v", err)
	}
	if out.Err != nil || out.Blocked || out.Aborted {
		t.Fatalf("分批执行应成功: %+v", out)
	}
	if out.RowsAffected != 2500 {
		t.Errorf("提交行数 = %d, 期望 2500", out.RowsAffected)
	}
	want := []int64{1000, 2000, 2500}
	if len(got) != len(want) {
		t.Fatalf("进度回调次数 = %d (%v), 期望 %d 次", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 次进度 = %d, 期望 %d（全部: %v）", i+1, got[i], want[i], got)
		}
	}

	if n := countBig(t, e, `v = 'done'`); n != 2500 {
		t.Errorf("被改行数 = %d, 期望 2500", n)
	}
}

// TestBulkBatchFailureKeepsEarlierBatches 验证「每批独立提交」这件事真的存在：
// 第二批失败时，第一批已经落到库里。
//
// 这条用例是分批功能的判别式：如果分批没接线（走单事务），第二批失败会整体回滚，
// 被改行数会是 0 而不是 1000。
func TestBulkBatchFailureKeepsEarlierBatches(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)
	prepBig(t, e, 2500)

	// 让 id=1500（落在第二批）的更新直接报错。
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `
		CREATE TRIGGER big_boom BEFORE UPDATE ON big FOR EACH ROW
		WHEN OLD.id = 1500
		BEGIN SELECT RAISE(ABORT, 'boom'); END`, nil); err != nil {
		t.Fatalf("建触发器: %v", err)
	}
	_ = s.Close()

	res, err := ex.BuildBulkUpdatePlan(ctx, service.BulkUpdateRequest{
		Table:            tableRef("big"),
		Set:              map[string]any{"v": "done"},
		ConfirmTable:     "big",
		AcceptNoRollback: true,
	})
	if err != nil {
		t.Fatalf("BuildBulkUpdatePlan: %v", err)
	}

	out, err := ex.ApplyChangePlan(ctx, res.Plan, service.Request{Approval: true})
	if err != nil {
		t.Fatalf("ApplyChangePlan: %v", err)
	}
	if out.Err == nil {
		t.Fatalf("第二批应失败, 实际成功: %+v", out)
	}
	if out.RowsAffected != 1000 {
		t.Errorf("已提交行数 = %d, 期望 1000（第一批）", out.RowsAffected)
	}
	if n := countBig(t, e, `v = 'done'`); n != 1000 {
		t.Errorf("被改行数 = %d, 期望 1000（只有第一批）", n)
	}
	if n := countBig(t, e, `id > 1000 AND v = 'done'`); n != 0 {
		t.Errorf("第二批不该有行被改, 实际 %d 行", n)
	}
}

// TestBulkBatchAbortBetweenBatches 验证中止：取消 ctx 后，已提交的批不回滚，
// 结果里明确标出「已中止」与已提交行数，而不是伪装成成功或失败。
func TestBulkBatchAbortBetweenBatches(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := newTestExecutor(t, e)
	prepBig(t, e, 2500)

	res, err := ex.BuildBulkUpdatePlan(ctx, service.BulkUpdateRequest{
		Table:            tableRef("big"),
		Set:              map[string]any{"v": "done"},
		ConfirmTable:     "big",
		AcceptNoRollback: true,
	})
	if err != nil {
		t.Fatalf("BuildBulkUpdatePlan: %v", err)
	}

	out, err := ex.ApplyChangePlanWithProgress(ctx, res.Plan, service.Request{Approval: true},
		func(done, total int64) {
			cancel() // 第一批提交后立刻要求停止
		})
	if err != nil {
		t.Fatalf("ApplyChangePlanWithProgress: %v", err)
	}
	if !out.Aborted {
		t.Fatalf("应标记为已中止: %+v", out)
	}
	if out.Blocked || out.Err != nil {
		t.Errorf("中止不是失败，不该同时标 Blocked/Err: %+v", out)
	}
	if out.RowsAffected != 1000 {
		t.Errorf("已提交行数 = %d, 期望 1000", out.RowsAffected)
	}
	if !strings.Contains(out.Reason, "已提交 1000 行") {
		t.Errorf("Reason 未讲清已提交行数: %q", out.Reason)
	}
	if n := countBig(t, e, `v = 'done'`); n != 1000 {
		t.Errorf("被改行数 = %d, 期望 1000", n)
	}
}

// TestBulkCompositePrimaryKeyDoesNotBatch 验证复合主键不启用分批，并且把原因写出来。
// 宁可少一个能力，也不能拼出各引擎方言不一的行值比较。
// TestBulkCompositePrimaryKeyBatches 验证复合主键也能按主键键集分批：
// 键集条件按列展开成 OR，不需要行值比较，三个引擎通吃。
//
// 批大小要比单列主键小得多（200 而不是 1000)：每多一组就要多两层表达式，
// SQLite 的表达式树深度上限是 1000，1000 组会直接报
// "Expression tree is too large"。
func TestBulkCompositePrimaryKeyBatches(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `
		CREATE TABLE comp (a INTEGER NOT NULL, b INTEGER NOT NULL, v TEXT, PRIMARY KEY (a, b))`, nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	// 每行两个不同的 (a,b) 组合，确保键集推进按元组比较而不是按单列。
	if _, err := s.Execute(ctx, `
		INSERT INTO comp (a, b, v)
		WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM seq WHERE x < 1500)
		SELECT x, (x % 3) + 1, 'x' FROM seq`, nil); err != nil {
		t.Fatalf("造数: %v", err)
	}
	_ = s.Close()

	res, err := ex.BuildBulkUpdatePlan(ctx, service.BulkUpdateRequest{
		Table:            tableRef("comp"),
		Set:              map[string]any{"v": "done"},
		Filter:           []service.FilterCond{{Column: "v", Op: service.FilterEq, Value: "x"}},
		AcceptNoRollback: true,
	})
	if err != nil {
		t.Fatalf("BuildBulkUpdatePlan: %v", err)
	}
	if res.Affected != 1500 {
		t.Fatalf("影响行数 = %d, 期望 1500", res.Affected)
	}
	if !res.Batched {
		t.Fatal("复合主键也应启用分批")
	}
	if res.BatchSize != 200 {
		t.Errorf("复合主键的批大小 = %d, 期望 200（受 SQLite 表达式树深度限制）", res.BatchSize)
	}
	if res.Batches != 8 {
		t.Errorf("批次数 = %d, 期望 8", res.Batches)
	}

	var progress []int64
	out, err := ex.ApplyChangePlanWithProgress(ctx, res.Plan, service.Request{Approval: true},
		func(done, total int64) { progress = append(progress, done) })
	if err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("分批执行失败: err=%v out=%+v", err, out)
	}
	if out.RowsAffected != 1500 {
		t.Errorf("提交行数 = %d, 期望 1500", out.RowsAffected)
	}
	if len(progress) != 8 {
		t.Errorf("进度回调 %d 次 (%v), 期望 8 次", len(progress), progress)
	}
	if progress[len(progress)-1] != 1500 {
		t.Errorf("最后一次进度 = %d, 期望 1500", progress[len(progress)-1])
	}

	// 逐行确认：不重不漏，key 全部命中且值都改了。
	s, err = e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	cur, err := s.Query(ctx, `SELECT COUNT(*) FROM comp WHERE v = 'done'`, nil, driver.QueryOptions{})
	if err != nil {
		t.Fatalf("核对: %v", err)
	}
	rows, _, err := cur.Next(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("核对: %v", err)
	}
	if n, _ := rows[0][0].(int64); n != 1500 {
		t.Errorf("被改行数 = %d, 期望 1500", n)
	}
}

// TestBulkBelowBatchSizeUsesSingleTransaction 验证小批量仍然走原来的单事务路径：
// 分批不是默认行为，只在真的需要时才启用（回滚语句依赖单事务的完整性）。
func TestBulkBelowBatchSizeUsesSingleTransaction(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)
	prepBig(t, e, service.DefaultBulkBatchSize)

	res, err := ex.BuildBulkUpdatePlan(ctx, service.BulkUpdateRequest{
		Table:  tableRef("big"),
		Set:    map[string]any{"v": "done"},
		Filter: []service.FilterCond{{Column: "v", Op: service.FilterEq, Value: "x"}},
	})
	if err != nil {
		t.Fatalf("BuildBulkUpdatePlan: %v", err)
	}
	if res.Affected != int64(service.DefaultBulkBatchSize) {
		t.Fatalf("影响行数 = %d, 期望 %d", res.Affected, service.DefaultBulkBatchSize)
	}
	if res.Batched {
		t.Error("正好等于单批上限时不该分批")
	}
	if res.Plan.Irreversible {
		t.Error("上限内的行数应能生成逐行回滚")
	}
	if len(res.Plan.Rollback) != service.DefaultBulkBatchSize {
		t.Errorf("回滚语句数 = %d, 期望 %d", len(res.Plan.Rollback), service.DefaultBulkBatchSize)
	}

	out, err := ex.ApplyChangePlan(ctx, res.Plan, service.Request{Approval: true})
	if err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("单事务执行失败: err=%v out=%+v", err, out)
	}
	if n := countBig(t, e, `v = 'done'`); n != int64(service.DefaultBulkBatchSize) {
		t.Errorf("被改行数 = %d, 期望 %d", n, service.DefaultBulkBatchSize)
	}
}
