package service

import (
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

// TestDiffSlowQueriesIsWindowDelta 钉住「累计值 → 窗口增量」（docs/09 §6.2 / 验收 3）。
//
// 这是 docs/09 点名最容易做错的地方：把累计值当窗口值展示，用户会看到「这条 SQL
// 跑了 120 万次」，而那是自实例启动以来的总量。
func TestDiffSlowQueriesIsWindowDelta(t *testing.T) {
	ex := &Executor{}

	// 第一次采样：只能给累计值，并标记 FirstSample。
	first := ex.diffSlow([]driver.QueryStat{
		{Digest: "a", Text: "SELECT * FROM t WHERE id = ?", Calls: 100, TotalMS: 5000},
		{Digest: "b", Text: "UPDATE u SET v = ?", Calls: 10, TotalMS: 9000},
	}, 20)
	if !first.FirstSample {
		t.Error("第一次采样必须标 first_sample")
	}
	if first.WindowSeconds != 0 {
		t.Errorf("首次采样没有窗口，window_seconds 应为 0，实际 %v", first.WindowSeconds)
	}
	// 按总耗时倒序：b(9000) 在 a(5000) 前面。
	if len(first.Items) != 2 || first.Items[0].Digest != "b" {
		t.Fatalf("首次采样应按总耗时倒序，实际 %+v", first.Items)
	}

	// 第二次采样：只给增量。a 多了 3 次 / 300ms，b 没动。
	// 间隔几毫秒：Windows 的时钟粒度是毫秒级，同一刻度内两次调用会算出 0 秒窗口。
	time.Sleep(5 * time.Millisecond)
	second := ex.diffSlow([]driver.QueryStat{
		{Digest: "a", Text: "SELECT * FROM t WHERE id = ?", Calls: 103, TotalMS: 5300},
		{Digest: "b", Text: "UPDATE u SET v = ?", Calls: 10, TotalMS: 9000},
	}, 20)
	if second.FirstSample {
		t.Error("第二次采样不该再标 first_sample")
	}
	if second.WindowSeconds <= 0 {
		t.Errorf("第二次采样应给出窗口跨度，实际 %v", second.WindowSeconds)
	}
	if len(second.Items) != 1 {
		t.Fatalf("窗口里没跑过的条目不该出现，实际 %+v", second.Items)
	}
	if got := second.Items[0]; got.Digest != "a" || got.Calls != 3 || got.TotalMS < 299 || got.TotalMS > 301 {
		t.Errorf("增量算错：%+v（期望 a / 3 次 / 300ms）", got)
	}
	if got := second.Items[0].MeanMS; got < 99 || got > 101 {
		t.Errorf("平均耗时应为 100ms，实际 %v", got)
	}
}

// TestDiffSlowQueriesSurvivesCounterReset 守住「计数器被清过」：差成负数时按新增处理。
//
// pg_stat_statements_reset 与 TRUNCATE performance_schema 都会把累计值清零；此时
// 相减是负数。显示负数比不显示更糟——它看起来像「这条语句跑了 -1073741824 次」。
func TestDiffSlowQueriesSurvivesCounterReset(t *testing.T) {
	ex := &Executor{}
	ex.diffSlow([]driver.QueryStat{{Digest: "a", Calls: 100, TotalMS: 5000}}, 20)
	got := ex.diffSlow([]driver.QueryStat{{Digest: "a", Calls: 5, TotalMS: 200}}, 20)
	if len(got.Items) != 1 {
		t.Fatalf("计数被清后应仍有这一条，实际 %+v", got.Items)
	}
	if got.Items[0].Calls != 5 || got.Items[0].TotalMS != 200 {
		t.Errorf("被清后应按新增处理（5 次 / 200ms），实际 %+v", got.Items[0])
	}
}
