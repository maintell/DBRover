package service

import (
	"fmt"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
)

// TestDiffSlowKeepsWiderBaseline 守住「窄查询不能冲掉宽查询的基线」。
//
// 真实路径：巡检只取 5 条慢查询、面板取 20 条。如果采样基线按本次结果**覆盖**，
// 面板下一次读时另外 15 条在基线里查不到，就会被当成「第一次见到」而拿**累计值**
// 冒充窗口增量——而且 first_sample 还是 false，等于骗人。这正是 docs/09 §6.2
// 点名的那个坑。
func TestDiffSlowKeepsWiderBaseline(t *testing.T) {
	ex := &Executor{}
	wide := make([]driver.QueryStat, 20)
	for i := range wide {
		wide[i] = driver.QueryStat{Digest: fmt.Sprintf("d%d", i), Calls: 10, TotalMS: 100}
	}

	// 第一次：面板取 20 条，建立宽基线。
	ex.diffSlow(wide, 20)
	// 第二次：巡检只取 5 条（不该把另外 15 条冲掉）。
	ex.diffSlow(wide[:5], 5)
	// 第三次：面板再取 20 条，中间没有任何新增。
	res := ex.diffSlow(wide, 20)

	if res.FirstSample {
		t.Fatal("已经有基线了，不该标首次采样")
	}
	if len(res.Items) != 0 {
		t.Fatalf("窗口内没有新增，应全部被差分掉，实际 %d 条：%+v", len(res.Items), res.Items)
	}
	for _, it := range res.Items {
		if it.Calls >= 10 {
			t.Errorf("把累计值当成增量了：%+v", it)
		}
	}
}
