package service

import (
	"context"
	"fmt"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

// HealthItem 是巡检里的一条结论。
type HealthItem struct {
	Name   string `json:"name"`
	Level  string `json:"level"` // ok | warn | crit | unknown
	Detail string `json:"detail"`
}

// HealthReport 是一次巡检的结果（FR-MON-07）。
//
// 分级口径：crit 是「现在就有事」，warn 是「再走一步就会出事」，unknown 是「这项
// 拿不到数据」——unknown 绝不能被算成 ok：那会把「没看到问题」冒充成「没有问题」。
type HealthReport struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Level       string       `json:"level"`
	Items       []HealthItem `json:"items"`
}

// healthInput 是四项采集结果，抽成结构是为了让 assess 能被单测直接打。
type healthInput struct {
	metrics *MetricsSummary
	locks   []driver.LockWait
	slow    *SlowQueriesResult
	sizes   []driver.TableSize
	errs    map[string]error
}

// HealthCheck 跑一组健康检查并出报告（FR-MON-07）。
//
// 单项采集失败**不影响**整份报告：那一项记 unknown 并写明原因，其余照常给结论。
// 巡检最怕的是「一项挂了整页白」，那等于在最需要它的时候罢工。
func (e *Executor) HealthCheck(ctx context.Context) (*HealthReport, error) {
	in := healthInput{errs: map[string]error{}}
	if e.conn.Capabilities().Metrics {
		m, err := e.Metrics(ctx)
		if err != nil {
			in.errs["连接与缓存"] = err
		} else {
			in.metrics = m
		}
	}
	if e.conn.Capabilities().LockGraph {
		l, err := e.LockWaits(ctx)
		if err != nil {
			in.errs["锁"] = err
		} else {
			in.locks = l
		}
	}
	if e.conn.Capabilities().SlowQueryStats {
		s, err := e.SlowQueries(ctx, 5)
		if err != nil {
			in.errs["慢查询"] = err
		} else {
			in.slow = s
		}
	}
	if e.conn.Capabilities().Capacity {
		z, err := e.Capacity(ctx, 5)
		if err != nil {
			in.errs["表体量"] = err
		} else {
			in.sizes = z
		}
	}
	return assess(in), nil
}

// assess 把采集结果变成分级结论。纯函数，便于构造数据直接断言规则。
func assess(in healthInput) *HealthReport {
	r := &HealthReport{GeneratedAt: time.Now(), Level: "ok"}
	add := func(name, level, detail string) {
		r.Items = append(r.Items, HealthItem{Name: name, Level: level, Detail: detail})
		r.Level = worse(r.Level, level)
	}

	// 1) 连接占用：过八成提醒，过九成算「现在就有事」。
	switch {
	case in.errs["连接与缓存"] != nil:
		add("连接与缓存", "unknown", "采集失败："+in.errs["连接与缓存"].Error())
	case in.metrics == nil:
		add("连接与缓存", "unknown", "这个引擎没有性能指标")
	case in.metrics.Degraded != "":
		add("连接与缓存", "unknown", in.metrics.Degraded)
	default:
		m := in.metrics
		if m.MaxConnections > 0 {
			pct := float64(m.Connections) / float64(m.MaxConnections) * 100
			switch {
			case pct > 90:
				add("连接占用", "crit", fmt.Sprintf("活跃 %d / 上限 %d（%.0f%%），已过九成", m.Connections, m.MaxConnections, pct))
			case pct > 80:
				add("连接占用", "warn", fmt.Sprintf("活跃 %d / 上限 %d（%.0f%%），已过八成", m.Connections, m.MaxConnections, pct))
			default:
				add("连接占用", "ok", fmt.Sprintf("活跃 %d / 上限 %d（%.0f%%）", m.Connections, m.MaxConnections, pct))
			}
		} else {
			add("连接占用", "unknown", fmt.Sprintf("活跃 %d，但拿不到上限", m.Connections))
		}
		if m.CacheHitRatio != nil {
			if *m.CacheHitRatio < 0.9 {
				add("缓存命中率", "warn", fmt.Sprintf("%.1f%%，低于 90%%", *m.CacheHitRatio*100))
			} else {
				add("缓存命中率", "ok", fmt.Sprintf("%.1f%%", *m.CacheHitRatio*100))
			}
		}
	}

	// 2) 锁等待：有等待就说明有人在排队；超过 3 条视为「现在就有事」。
	switch {
	case in.errs["锁"] != nil:
		add("锁等待", "unknown", "采集失败："+in.errs["锁"].Error())
	case in.locks == nil:
		add("锁等待", "unknown", "这个引擎没有锁等待视图")
	case len(in.locks) == 0:
		add("锁等待", "ok", "当前没有锁等待")
	case len(in.locks) > 3:
		add("锁等待", "crit", fmt.Sprintf("%d 条等待，最长 %d 秒", len(in.locks), maxWait(in.locks)))
	default:
		add("锁等待", "warn", fmt.Sprintf("%d 条等待，最长 %d 秒", len(in.locks), maxWait(in.locks)))
	}

	// 3) 慢查询：首次采样只有累计值，此时给 unknown 而不是假装「很慢」。
	switch {
	case in.errs["慢查询"] != nil:
		add("慢查询", "unknown", "采集失败："+in.errs["慢查询"].Error())
	case in.slow == nil:
		add("慢查询", "unknown", "这个引擎没有慢查询统计")
	case !in.slow.Available:
		add("慢查询", "unknown", "实例没开慢查询统计")
	case in.slow.Degraded != "":
		add("慢查询", "unknown", in.slow.Degraded)
	case in.slow.FirstSample:
		add("慢查询", "unknown", "首次采样，暂无增量")
	case len(in.slow.Items) == 0:
		add("慢查询", "ok", "这段时间没有慢查询")
	default:
		top := in.slow.Items[0]
		add("慢查询", "warn", fmt.Sprintf("%d 条，最慢一条 %.0fms（%d 次）", len(in.slow.Items), top.TotalMS, top.Calls))
	}

	// 4) 表体量：单表过 1GB 提醒。阈值写死并写进文档——拍脑袋的可配项比没有更糟。
	switch {
	case in.errs["表体量"] != nil:
		add("表体量", "unknown", "采集失败："+in.errs["表体量"].Error())
	case len(in.sizes) == 0:
		add("表体量", "unknown", "拿不到表体量")
	default:
		big := in.sizes[0]
		const oneGB = 1 << 30
		if big.TotalBytes >= oneGB {
			add("表体量", "warn", fmt.Sprintf("最大表 %s 已过 1GB", big.Name))
		} else {
			add("表体量", "ok", fmt.Sprintf("最大表 %s 占 %s", big.Name, humanBytes(big.TotalBytes)))
		}
	}

	return r
}

// worse 取两个级别里更严重的那个。
func worse(a, b string) string {
	rank := map[string]int{"ok": 0, "unknown": 1, "warn": 2, "crit": 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// humanBytes 按量级选单位。
//
// 固定用 MB 会把 16KB 的小表写成「0 MB」——那读起来像「没有数据」。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func maxWait(items []driver.LockWait) int64 {
	var m int64
	for _, it := range items {
		if it.WaitSeconds > m {
			m = it.WaitSeconds
		}
	}
	return m
}
