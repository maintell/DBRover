package service

import (
	"testing"
	"time"
)

// TestGrowthOf 钉住「没有历史就不编数字」，以及两个点时的差值口径。
func TestGrowthOf(t *testing.T) {
	// 一个点：只能说明现在多大，不能算增长。
	if _, _, ok := GrowthOf([]MetricPoint{{TS: 1, Value: 100}}, 200); ok {
		t.Error("只有一个点时不该给增长")
	}
	if _, _, ok := GrowthOf(nil, 200); ok {
		t.Error("没有点时不该给增长")
	}

	// 两个点：差值 = 当前 - 最早那个点，跨度按最早点算。
	old := time.Now().Add(-3 * time.Hour).Unix()
	delta, hours, ok := GrowthOf([]MetricPoint{
		{TS: old, Value: 1000},
		{TS: time.Now().Add(-time.Hour).Unix(), Value: 1500},
	}, 4096)
	if !ok {
		t.Fatal("两个点时应当能给增长")
	}
	if delta != 3096 {
		t.Errorf("增长应为 4096-1000=3096，实际 %d", delta)
	}
	if hours < 2.9 || hours > 3.1 {
		t.Errorf("跨度应约 3 小时，实际 %v", hours)
	}
}
