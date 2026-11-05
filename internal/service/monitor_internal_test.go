package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

// TestSummarizeCountersRules 钉住「结论由规则产出」（docs/09 §8 验收 2）。
//
// 固定文案的结论永远看不出对错、也永远测不出来；这里构造数据断言规则真的会触发。
func TestSummarizeCountersRules(t *testing.T) {
	cases := []struct {
		name       string
		in         driver.Counters
		status     string
		verdictHas string
		detailHas  string
	}{
		{
			name:       "正常",
			in:         driver.Counters{Connections: 12, MaxConnections: 200, CacheHits: 990, CacheMisses: 10},
			status:     "ok",
			verdictHas: "12 / 上限 200（6%）",
		},
		{
			name:      "连接过八成",
			in:        driver.Counters{Connections: 170, MaxConnections: 200, CacheHits: 990, CacheMisses: 10},
			status:    "warn",
			detailHas: "八成",
		},
		{
			name:      "命中率低于 90%",
			in:        driver.Counters{Connections: 5, MaxConnections: 200, CacheHits: 800, CacheMisses: 200},
			status:    "warn",
			detailHas: "低于 90%",
		},
		{
			name:       "什么都取不到",
			in:         driver.Counters{},
			status:     "unknown",
			verdictHas: "没有可用的负载与缓存指标",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := summarizeCounters(c.in)
			if got.Status != c.status {
				t.Errorf("Status = %q, 期望 %q（Verdict=%q）", got.Status, c.status, got.Verdict)
			}
			if c.verdictHas != "" && !strings.Contains(got.Verdict, c.verdictHas) {
				t.Errorf("Verdict = %q, 期望包含 %q", got.Verdict, c.verdictHas)
			}
			if c.detailHas != "" {
				joined := strings.Join(got.Details, "；")
				if !strings.Contains(joined, c.detailHas) {
					t.Errorf("Details = %q, 期望包含 %q", joined, c.detailHas)
				}
			}
		})
	}

	// 命中率的算法本身也要钉住：990/(990+10) = 99%。
	s := summarizeCounters(driver.Counters{Connections: 1, MaxConnections: 100, CacheHits: 990, CacheMisses: 10})
	if s.CacheHitRatio == nil {
		t.Fatal("有计数时应当给出命中率")
	}
	if *s.CacheHitRatio < 0.989 || *s.CacheHitRatio > 0.991 {
		t.Errorf("命中率 = %v, 期望 0.99", *s.CacheHitRatio)
	}
	// 两边都是 0 时不能算出 0% 或 100% —— 那是「没数据」，不是「全不命中」。
	empty := summarizeCounters(driver.Counters{Connections: 1, MaxConnections: 100})
	if empty.CacheHitRatio != nil {
		t.Errorf("没有计数时不该给命中率，实际 %v", *empty.CacheHitRatio)
	}
}

// blockingProvider 是一个「永远慢」的指标源，用来验证超时分支。
type blockingProvider struct{ wait time.Duration }

func (p blockingProvider) Counters(ctx context.Context) (driver.Counters, error) {
	select {
	case <-time.After(p.wait):
		return driver.Counters{Connections: 1}, nil
	case <-ctx.Done():
		return driver.Counters{}, ctx.Err()
	}
}

// TestCollectMetricsDegradesOnTimeout 钉住 docs/09 §2.1 / §8 验收 8：
// 采集超时**不算错误**，记一次降级并在界面上标注。
func TestCollectMetricsDegradesOnTimeout(t *testing.T) {
	sum, _, err := collectMetrics(context.Background(), blockingProvider{wait: 2 * time.Second}, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("超时不该返回错误: %v", err)
	}
	if sum.Degraded == "" {
		t.Error("超时后必须标注降级，否则界面会把「没数据」画成「一切正常」")
	}
	if sum.Status != "unknown" {
		t.Errorf("Status = %q, 期望 unknown", sum.Status)
	}
	if sum.Connections != 0 {
		t.Errorf("降级时不该编造读数，实际 connections=%d", sum.Connections)
	}
}

// TestApplyQPSSkipsFirstSample 钉住 QPS 的两条规矩。
//
// 首次采样没有上一份样本 → 不给数字，而不是拿「自启动以来的平均值」冒充；计数器
// 被重置时差为负 → 这一轮跳过（显示负数比不显示更糟）。
func TestApplyQPSSkipsFirstSample(t *testing.T) {
	ex := &Executor{}
	t0 := time.Now()

	s1 := &MetricsSummary{}
	ex.applyQPS(s1, driver.Counters{Statements: 100, CollectedAt: t0})
	if s1.QPS != nil {
		t.Error("首次采样不该给 QPS")
	}

	s2 := &MetricsSummary{}
	ex.applyQPS(s2, driver.Counters{Statements: 300, CollectedAt: t0.Add(2 * time.Second)})
	if s2.QPS == nil || *s2.QPS < 99 || *s2.QPS > 101 {
		t.Errorf("QPS 应为 100（2 秒 200 条），实际 %v", s2.QPS)
	}

	s3 := &MetricsSummary{}
	ex.applyQPS(s3, driver.Counters{Statements: 5, CollectedAt: t0.Add(4 * time.Second)})
	if s3.QPS != nil {
		t.Errorf("计数器被重置时不该给 QPS，实际 %v", *s3.QPS)
	}
}

// TestCollectMetricsPassesThrough 确认正常路径不被降级逻辑吃掉。
func TestCollectMetricsPassesThrough(t *testing.T) {
	fast := blockingProvider{wait: 0}
	sum, _, err := collectMetrics(context.Background(), fast, time.Second)
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if sum.Degraded != "" {
		t.Errorf("不该标注降级: %q", sum.Degraded)
	}
	if sum.Connections != 1 {
		t.Errorf("connections = %d, 期望 1", sum.Connections)
	}
}
