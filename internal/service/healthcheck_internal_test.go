package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
)

// TestAssessHealthRules 钉住巡检的四条规则与「未知不等于正常」。
func TestAssessHealthRules(t *testing.T) {
	ratio := 0.95
	ok := assess(healthInput{
		metrics: &MetricsSummary{Connections: 1, MaxConnections: 100, CacheHitRatio: &ratio},
		locks:   []driver.LockWait{},
		slow:    &SlowQueriesResult{Available: true},
		sizes:   []driver.TableSize{{Name: "t", TotalBytes: 1 << 20}},
	})
	if ok.Level != "ok" {
		t.Errorf("全绿时整体应为 ok，实际 %q：%+v", ok.Level, ok.Items)
	}
	// 五项：连接占用、缓存命中率、锁等待、慢查询、表体量（缓存是独立的一项，
	// 它和连接数回答的不是同一个问题）。
	if len(ok.Items) != 5 {
		t.Errorf("应有五项结论，实际 %d", len(ok.Items))
	}

	// 有锁等待 → warn，且整体跟着升。
	warn := assess(healthInput{
		metrics: &MetricsSummary{Connections: 1, MaxConnections: 100, CacheHitRatio: &ratio},
		locks:   []driver.LockWait{{WaitingID: "5", BlockingID: "4", WaitSeconds: 7}},
		slow:    &SlowQueriesResult{Available: true},
		sizes:   []driver.TableSize{{Name: "t", TotalBytes: 1 << 20}},
	})
	if warn.Level != "warn" {
		t.Errorf("有锁等待应升到 warn，实际 %q", warn.Level)
	}

	// 采集失败 → 那项 unknown，整体也是 unknown；**不能落回 ok**。
	unknown := assess(healthInput{
		errs: map[string]error{"连接与缓存": errors.New("超时")},
		slow: &SlowQueriesResult{Available: true},
	})
	if unknown.Level != "unknown" {
		t.Errorf("采集失败时整体应为 unknown（不能把「没看到问题」冒充成「没问题」），实际 %q", unknown.Level)
	}
	found := false
	for _, it := range unknown.Items {
		if it.Name == "连接与缓存" && it.Level == "unknown" && strings.Contains(it.Detail, "超时") {
			found = true
		}
	}
	if !found {
		t.Errorf("应有一条「连接与缓存 / unknown / 含原因」的结论，实际 %+v", unknown.Items)
	}

	// 慢查询首次采样只有累计值 → unknown，不假装「很慢」也不假装「很快」。
	first := assess(healthInput{
		metrics: &MetricsSummary{Connections: 1, MaxConnections: 100},
		locks:   []driver.LockWait{},
		slow:    &SlowQueriesResult{Available: true, FirstSample: true},
	})
	for _, it := range first.Items {
		if it.Name == "慢查询" && it.Level != "unknown" {
			t.Errorf("首次采样的慢查询应为 unknown，实际 %+v", it)
		}
	}
}
