package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

// prevStat 是一次采样里某条指纹的累计值。
type prevStat struct {
	calls   int64
	totalMS float64
	rows    int64
}

// slowSampler 保存上一次采样的累计值，用来算窗口增量。
//
// 挂在 Executor 上（每条连接一个），天然按连接隔离——两个库的指纹互不相干。
type slowSampler struct {
	mu   sync.Mutex
	prev map[string]prevStat
	at   time.Time
}

// SlowQuery 是一条慢查询。**数值是窗口内的增量**，不是自启动以来的累计量。
type SlowQuery struct {
	Digest  string  `json:"digest"`
	Text    string  `json:"text"`
	Calls   int64   `json:"calls"`
	TotalMS float64 `json:"total_ms"`
	MeanMS  float64 `json:"mean_ms"`
	Rows    int64   `json:"rows"`
}

// SlowQueriesResult 是一次慢查询查询的结果。
type SlowQueriesResult struct {
	Items []SlowQuery `json:"items"`
	// FirstSample 为 true 表示这是第一次采样：Items 里是**累计值**，没有增量。
	// 界面必须照 docs/09 §6.2 标注「首次采样，暂无增量」，否则用户会把自启动以来
	// 的总量当成最近这段窗口的量。
	FirstSample bool `json:"first_sample"`
	// WindowSeconds 是本次增量覆盖的时间跨度；首次采样为 0。
	WindowSeconds float64   `json:"window_seconds"`
	CollectedAt   time.Time `json:"collected_at"`
	// Available 为 false 表示这个实例没开慢查询统计，Hint 里写着怎么打开。
	Available bool   `json:"available"`
	Hint      string `json:"hint,omitempty"`
	// Degraded 非空表示这次没采到（超时），且不是错误。
	Degraded string `json:"degraded,omitempty"`
}

// SlowQueries 读一次慢查询并差分成窗口增量（FR-MON-04）。
//
// 第一次调用只能给累计值并标记 FirstSample：引擎的计数器是累计的，没有上一份
// 样本就算不出增量。谎报「最近 15 分钟」比不报更糟。
func (e *Executor) SlowQueries(ctx context.Context, limit int) (*SlowQueriesResult, error) {
	if !e.conn.Capabilities().SlowQueryStats {
		return nil, fmt.Errorf("%s 没有慢查询统计", e.conn.Engine())
	}
	sp, ok := e.conn.(driver.SlowQueryProvider)
	if !ok {
		return nil, fmt.Errorf("能力位声明了慢查询统计，但 %s 驱动没有实现 SlowQueryProvider", e.conn.Engine())
	}
	cctx, cancel := context.WithTimeout(ctx, metricsCollectionTimeout)
	defer cancel()
	stats, err := sp.QueryStats(cctx, limit)
	if err != nil {
		if errors.Is(err, driver.ErrSlowQueryUnavailable) {
			return &SlowQueriesResult{Available: false, Hint: sp.Hint()}, nil
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return &SlowQueriesResult{Available: true,
				Degraded: fmt.Sprintf("采集超过 %s 未返回，已放弃（监控不能把库拖慢）", metricsCollectionTimeout)}, nil
		}
		return nil, err
	}
	return e.diffSlow(stats, limit), nil
}

// diffSlow 把累计值差分成窗口增量。抽成方法是为了让「差分」这件事能被单测直接打。
func (e *Executor) diffSlow(stats []driver.QueryStat, limit int) *SlowQueriesResult {
	now := time.Now()
	e.slow.mu.Lock()
	defer e.slow.mu.Unlock()

	prev, had := e.slow.prev, e.slow.prev != nil
	prevAt := e.slow.at
	cur := make(map[string]prevStat, len(stats))
	items := make([]SlowQuery, 0, len(stats))
	for _, st := range stats {
		cur[st.Digest] = prevStat{calls: st.Calls, totalMS: st.TotalMS, rows: st.Rows}
		it := SlowQuery{Digest: st.Digest, Text: st.Text, Calls: st.Calls, TotalMS: st.TotalMS, Rows: st.Rows}
		if had {
			if p, ok := prev[st.Digest]; ok {
				it.Calls = st.Calls - p.calls
				it.TotalMS = st.TotalMS - p.totalMS
				it.Rows = st.Rows - p.rows
			}
			// 计数器会被重置（PG 的 pg_stat_statements_reset、MySQL 的 TRUNCATE P_S）。
			// 差成负数说明「这中间被清过」，按新增处理而不是报错，更不能显示负数。
			if it.Calls < 0 {
				it.Calls = st.Calls
			}
			if it.TotalMS < 0 {
				it.TotalMS = st.TotalMS
			}
			if it.Rows < 0 {
				it.Rows = st.Rows
			}
		}
		if it.Calls > 0 {
			it.MeanMS = it.TotalMS / float64(it.Calls)
		}
		items = append(items, it)
	}
	// 基线**只增不删**：巡检只取 5 条、面板取 20 条，若按本次结果覆盖整张表，
	// 下一次面板读就会把另外 15 条当成「第一次见到」而拿累计值冒充窗口增量
	// （first_sample 还是 false，等于骗人）。合并的代价只是表里留一些陈旧的指纹。
	if e.slow.prev == nil {
		e.slow.prev = make(map[string]prevStat, len(cur))
	}
	for k, v := range cur {
		e.slow.prev[k] = v
	}
	e.slow.at = now

	res := &SlowQueriesResult{Items: items, FirstSample: !had, CollectedAt: now, Available: true}
	if had {
		res.WindowSeconds = now.Sub(prevAt).Seconds()
		// 只留下窗口内真的跑过的：差分为 0 的条目在「最近这段」里没发生，列出来只会占位。
		kept := items[:0]
		for _, it := range items {
			if it.Calls > 0 {
				kept = append(kept, it)
			}
		}
		res.Items = kept
	}
	// 按**窗口内**总耗时倒序。引擎给的是自启动以来的排序，窗口里跑得最多的可能
	// 根本不在前几名；不重排就等于用错的顺序展示对的数据（docs/09 验收 4）。
	sort.SliceStable(res.Items, func(i, j int) bool { return res.Items[i].TotalMS > res.Items[j].TotalMS })
	if limit > 0 && len(res.Items) > limit {
		res.Items = res.Items[:limit]
	}
	return res
}
