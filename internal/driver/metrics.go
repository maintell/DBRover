package driver

import (
	"context"
	"time"
)

// Counters 是一次「连接与负载 / 缓存命中」的即时读数（FR-MON-01）。
//
// 只放两个引擎都真有对应物的项：
//   - 连接数是瞬时值，直接取；
//   - 缓存命中与未命中是**累计**计数，比率由上层算（hit/(hit+miss)）。这一项
//     **不需要差分**——两份累计值相除得到的「自启动以来命中率」本身就是可用指标
//     （docs/09 §3.1 给的就是这个公式）。
//
// 刻意不放 QPS 与慢查询：它们也是累计量，必须两次采样做差分（docs/09 §6.2），
// 属于采样器那一层的事。把「瞬时值」与「必须差分的量」塞进同一个结构，迟早有人
// 把「自启动以来的总量」当成「最近一分钟的量」展示出去——docs/09 点名这是最
// 容易做错的地方。
type Counters struct {
	// Connections 是当前连接数（含本工具自己这条：两个引擎的原生计数都含，
	// 减掉反而要与 Sessions 的口径对齐，收益不抵复杂度）。
	Connections int `json:"connections"`
	// MaxConnections 是引擎允许的连接上限；0 表示这次没取到，不是「上限为零」。
	MaxConnections int `json:"max_connections"`
	// CacheHits / CacheMisses 是缓存命中与未命中的累计次数。
	// PG 取 blks_hit / blks_read；MySQL 取 buffer pool 的 read_requests / reads。
	CacheHits   int64 `json:"cache_hits"`
	CacheMisses int64 `json:"cache_misses"`
	// UptimeSeconds 是实例已运行的秒数；0 表示没取到。
	UptimeSeconds int64 `json:"uptime_seconds"`
	// CollectedAt 是采集时刻（服务端时钟），让上层知道这份读数有多旧。
	CollectedAt time.Time `json:"collected_at"`
}

// MetricsProvider 是「性能面板」的可选能力（FR-MON-01）。
//
// 可选而不是主接口，理由同 MonitorProvider：没有这套服务端计数器的引擎（SQLite
// 是进程内的单文件，没有全局统计）不该为了实现而实现。判据是 Capabilities().Metrics。
type MetricsProvider interface {
	// Counters 读一次当前计数。
	//
	// 实现必须让查询可被 ctx 取消：上层会给一个较短的超时（默认 3s），超时按
	// 「降级」处理而不是错误——监控把库拖慢比看不到数字更糟（docs/09 §2.1）。
	Counters(ctx context.Context) (Counters, error)
}
