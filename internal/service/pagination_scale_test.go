package service_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// bigTableRows 是这套压测的行数。
//
// 100 万行是 docs/05 验收 5 的原始口径。SQLite 在一个事务里灌 100 万行大约几秒，
// 值得按真实规模跑而不是拿小表外推——外推出来的数字不能当证据。
const bigTableRows = 1_000_000

// seedBigTable 在一个事务里灌 n 行。
//
// 逐条 Execute 会在每次提交时 fsync，100 万行能跑几分钟；这里用一个递归 CTE
// 一次性插入，是 SQLite 上最快的做法。
func seedBigTable(t *testing.T, e *env, n int) {
	t.Helper()
	ctx := context.Background()
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.Execute(ctx,
		`CREATE TABLE big (id INTEGER PRIMARY KEY, bucket TEXT, n INTEGER)`, nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	q := `WITH RECURSIVE seq(x) AS (
	  SELECT 1 UNION ALL SELECT x + 1 FROM seq WHERE x < ?
	)
	INSERT INTO big (id, bucket, n)
	SELECT x, 'b' || (x % 7), x FROM seq`
	if _, err := s.Execute(ctx, q, []any{n}); err != nil {
		t.Fatalf("灌数据: %v", err)
	}
}

// TestMillionRowPagination 对应 docs/05 验收 5：
// 100 万行翻页不 OOM，内存增长 < 200MB。
func TestMillionRowPagination(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 下跳过百万行压测")
	}
	e := setup(t)
	seedBigTable(t, e, bigTableRows)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	// 量的是「翻页过程中的额外占用」，所以先 GC 一次拿基线。
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	const pageSize = 200
	const maxPages = 60 // 只走前 1.2 万行，验证「长时间翻页」而不是把 100 万行全拉一遍

	ctx := context.Background()
	var after []any
	total := 0
	start := time.Now()
	for page := 0; page < maxPages; page++ {
		res, err := ex.TableRows(ctx, service.TableRowsRequest{
			Table: "big", Limit: pageSize, After: after,
		})
		if err != nil {
			t.Fatalf("第 %d 页失败: %v", page, err)
		}
		total += len(res.Rows)
		if !res.HasMore {
			break
		}
		after = res.Next
	}
	elapsed := time.Since(start)

	var afterStats runtime.MemStats
	runtime.ReadMemStats(&afterStats)
	// HeapAlloc 是「活着的对象」，比 TotalAlloc 更能反映真实占用。
	var growthMB float64
	if afterStats.HeapAlloc > before.HeapAlloc {
		growthMB = float64(afterStats.HeapAlloc-before.HeapAlloc) / (1 << 20)
	}

	t.Logf("翻了 %d 页、%d 行，总耗时 %v，单页均摊 %v，HeapAlloc 增长 %.1f MB",
		maxPages, total, elapsed, elapsed/maxPages, growthMB)

	if total != maxPages*pageSize {
		t.Errorf("取回 %d 行，期望 %d", total, maxPages*pageSize)
	}
	if growthMB > 200 {
		t.Errorf("内存增长 %.1f MB，超过 200MB 上限", growthMB)
	}
}

// TestDeepKeysetPageLatency 对应 docs/05 验收 6：
// 深分页（定位到很靠后的一段）单次 < 500ms。
//
// 键集分页没有 OFFSET，所以「第 10000 页」的代价等价于「从一个很靠后的键开始取一页」。
// 这里直接拿中段的定位值，避免为了走到那里先翻一万次（那测的是循环不是查询）。
func TestDeepKeysetPageLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 下跳过深分页基准")
	}
	e := setup(t)
	seedBigTable(t, e, bigTableRows)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	// 从 50 万行之后开始取——相当于翻了 2500 页（每页 200 行）之后的位置。
	after := []any{int64(bigTableRows / 2)}
	start := time.Now()
	res, err := ex.TableRows(context.Background(), service.TableRowsRequest{
		Table: "big", Limit: 200, After: after,
	})
	if err != nil {
		t.Fatalf("深分页失败: %v", err)
	}
	elapsed := time.Since(start)

	t.Logf("从第 %d 行之后取一页：%d 行，耗时 %v", bigTableRows/2, len(res.Rows), elapsed)
	if len(res.Rows) != 200 {
		t.Errorf("应取满 200 行，实际 %d", len(res.Rows))
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("深分页耗时 %v，超过 500ms", elapsed)
	}
}
