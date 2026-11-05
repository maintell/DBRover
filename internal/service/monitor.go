package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

// metricsCollectionTimeout 是采集超时（docs/09 §2.1：默认 3s）。
//
// 是变量而不是常量，只为一件事：单元测试要把它缩短，否则验证「超时降级」这条
// 得让每个用例都干等 3 秒。
var metricsCollectionTimeout = 3 * time.Second

// MetricsSummary 是「连接与负载」「缓存命中」两张卡的读数与结论（FR-MON-01）。
//
// 结论（Status / Verdict / Details）全部由规则产出，不是固定文案——固定文案
// 永远看不出对错，也永远测不出来。
type MetricsSummary struct {
	Connections    int       `json:"connections"`
	MaxConnections int       `json:"max_connections"`
	CacheHitRatio  *float64  `json:"cache_hit_ratio,omitempty"`
	UptimeSeconds  int64     `json:"uptime_seconds"`
	CollectedAt    time.Time `json:"collected_at"`
	// Status 是规则产出的整体状态：ok / warn / unknown。
	Status string `json:"status"`
	// Verdict 是一句结论，同样由规则产出。
	Verdict string `json:"verdict"`
	// Details 是被触发的规则明细，供「点进详情」用。
	Details []string `json:"details,omitempty"`
	// Degraded 非空表示这次没拿到数据（例如采集超时），且**不是错误**。
	// 监控拿不到数字不该报 5xx —— 那只会让人以为库挂了。
	Degraded string `json:"degraded,omitempty"`
}

// Metrics 读一次性能指标并给出结论。
//
// 超时按降级处理：返回 200 + Degraded，而不是错误。
func (e *Executor) Metrics(ctx context.Context) (*MetricsSummary, error) {
	if !e.conn.Capabilities().Metrics {
		return nil, fmt.Errorf("%s 没有性能指标", e.conn.Engine())
	}
	mp, ok := e.conn.(driver.MetricsProvider)
	if !ok {
		return nil, fmt.Errorf("能力位声明了性能指标，但 %s 驱动没有实现 MetricsProvider", e.conn.Engine())
	}
	return collectMetrics(ctx, mp, metricsCollectionTimeout)
}

// collectMetrics 采集并处理降级。
//
// 单独拆出来是为了能用假的 MetricsProvider 直接测超时分支——为了一个分支去实现
// 整个 driver.Connector 是一笔不划算的交易，而超时这条恰恰是最需要钉住的：
// docs/09 §2.1 要求「超时不算错误，记一次降级并在 UI 标注」，写错的代价是运维
// 在最需要面板的时候看到一个 5xx。
func collectMetrics(ctx context.Context, mp driver.MetricsProvider, timeout time.Duration) (*MetricsSummary, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	counters, err := mp.Counters(cctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return &MetricsSummary{
				Status:   "unknown",
				Verdict:  "采集超时，本次不展示指标",
				Degraded: fmt.Sprintf("采集超过 %s 未返回，已放弃（监控不能把库拖慢）", timeout),
			}, nil
		}
		return nil, err
	}
	return summarizeCounters(counters), nil
}

// summarizeCounters 把读数变成「状态 + 一句结论」。
//
// 规则口径见 docs/09 §3.1：连接数过八成上限、缓存命中率低于 90% 各触发一条警告。
// 抽成纯函数是为了让「结论由规则产出」可被单测断言。
func summarizeCounters(c driver.Counters) *MetricsSummary {
	s := &MetricsSummary{
		Connections:    c.Connections,
		MaxConnections: c.MaxConnections,
		UptimeSeconds:  c.UptimeSeconds,
		CollectedAt:    c.CollectedAt,
		Status:         "ok",
	}
	haveLoad := c.MaxConnections > 0
	if sum := c.CacheHits + c.CacheMisses; sum > 0 {
		r := float64(c.CacheHits) / float64(sum)
		s.CacheHitRatio = &r
	}
	haveCache := s.CacheHitRatio != nil

	if haveLoad {
		pct := float64(c.Connections) / float64(c.MaxConnections) * 100
		s.Verdict = fmt.Sprintf("活跃连接 %d / 上限 %d（%.0f%%）", c.Connections, c.MaxConnections, pct)
		if pct > 80 {
			s.Status = "warn"
			s.Details = append(s.Details, fmt.Sprintf("连接数已过八成上限（%.0f%%）", pct))
		}
	} else if c.Connections > 0 {
		s.Verdict = fmt.Sprintf("活跃连接 %d（上限未知）", c.Connections)
	}
	if haveCache && *s.CacheHitRatio < 0.9 {
		s.Status = "warn"
		s.Details = append(s.Details, fmt.Sprintf("缓存命中率 %.1f%%，低于 90%%", *s.CacheHitRatio*100))
	}
	if !haveLoad && !haveCache {
		s.Status = "unknown"
		s.Verdict = "这个引擎没有可用的负载与缓存指标"
	}
	return s
}
