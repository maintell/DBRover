package pg

import (
	"context"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

// Counters 读连接数与缓存命中（FR-MON-01）。
//
// 只查一张轻量系统视图，并按当前库收窄：pg_stat_database 是集群级的，把所有库
// 加起来会得到一个跨库混合命中率——看不出是哪个库在打磁盘。
//
// 连接数用 numbackends（当前库的连接数），不是全实例连接数：这个面板是「这个
// 连接所在的库」的负载，混入别的库会误导。
func (c *Connector) Counters(ctx context.Context) (driver.Counters, error) {
	h, err := c.handle()
	if err != nil {
		return driver.Counters{}, err
	}
	var out driver.Counters
	err = h.QueryRowContext(ctx, `
		SELECT d.numbackends,
		       COALESCE((SELECT setting::int FROM pg_settings WHERE name = 'max_connections'), 0),
		       d.blks_hit, d.blks_read,
		       COALESCE(EXTRACT(EPOCH FROM (now() - pg_postmaster_start_time()))::bigint, 0)
		  FROM pg_stat_database d
		 WHERE d.datname = current_database()`).Scan(
		&out.Connections, &out.MaxConnections, &out.CacheHits, &out.CacheMisses, &out.UptimeSeconds)
	if err != nil {
		return driver.Counters{}, err
	}
	// 语句总数只有装了 pg_stat_statements 才有。取不到就留 -1（QPS 显示「未知」），
	// 千万别留 0——0 在差分里意味着「这段时间一条语句都没跑」，那是另一回事。
	out.Statements = -1
	var total int64
	if serr := h.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(calls), -1)::bigint FROM pg_stat_statements").Scan(&total); serr == nil {
		out.Statements = total
	}
	out.CollectedAt = time.Now()
	return out, nil
}
