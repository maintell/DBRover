package mysql

import (
	"context"
	"strconv"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

// Counters 读连接数与缓存命中（FR-MON-01）。
//
// 用一次 SHOW GLOBAL STATUS 取全部再挑三项，而不是连发三条 SHOW：少两次往返，
// 也避免三条之间数字不同步——连接数与命中数本来就该是同一时刻的读数。
//
// 缓存用 InnoDB buffer pool 的两个计数器：read_requests 是「要读的页数」，
// reads 是「真的要去磁盘读的页数」，命中率 = 1 - reads/read_requests。
func (c *Connector) Counters(ctx context.Context) (driver.Counters, error) {
	h, err := c.handle()
	if err != nil {
		return driver.Counters{}, err
	}
	rows, err := h.QueryContext(ctx, `SHOW GLOBAL STATUS`)
	if err != nil {
		return driver.Counters{}, err
	}
	defer func() { _ = rows.Close() }()

	out := driver.Counters{Statements: -1}
	for rows.Next() {
		var name, val string
		if err := rows.Scan(&name, &val); err != nil {
			return driver.Counters{}, err
		}
		switch name {
		case `Threads_connected`:
			if v, cerr := strconv.Atoi(val); cerr == nil {
				out.Connections = v
			}
		case `Innodb_buffer_pool_read_requests`:
			if v, cerr := strconv.ParseInt(val, 10, 64); cerr == nil {
				out.CacheHits = v
			}
		case `Innodb_buffer_pool_reads`:
			if v, cerr := strconv.ParseInt(val, 10, 64); cerr == nil {
				out.CacheMisses = v
			}
		case `Questions`:
			// 累计语句数（客户端发出的语句，不含复制线程）。QPS 由上层差分算。
			if v, cerr := strconv.ParseInt(val, 10, 64); cerr == nil {
				out.Statements = v
			}
		case `Uptime`:
			if v, cerr := strconv.ParseInt(val, 10, 64); cerr == nil {
				out.UptimeSeconds = v
			}
		}
	}
	if err := rows.Err(); err != nil {
		return driver.Counters{}, err
	}
	// 上限单独查：它在 SHOW VARIABLES 里，不在 SHOW STATUS 里。取不到不影响其余读数。
	var maxConn int
	if err := h.QueryRowContext(ctx, `SELECT @@max_connections`).Scan(&maxConn); err == nil {
		out.MaxConnections = maxConn
	}
	out.CollectedAt = time.Now()
	return out, nil
}
