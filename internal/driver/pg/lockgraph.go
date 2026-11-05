package pg

import (
	"context"
	"strconv"

	"github.com/maintell/DBRover/internal/driver"
)

// lockWaitSQL 取锁等待边。
//
// pg_blocking_pids(pid) 直接给「挡住这个会话的那些 pid」，是由 PG 自己算的（含
// 软阻塞与并行 worker 的处理），比自己拿 pg_locks 拼准确得多。cardinality 判断
// 放在 WHERE 里：没有等待者时这个函数返回空数组，不加过滤会得到一堆 NULL 行。
const lockWaitSQL = "SELECT w.pid, b.pid, " +
	"COALESCE(EXTRACT(EPOCH FROM (now() - w.query_start))::bigint, -1), " +
	"COALESCE(w.query, ''), COALESCE(b.query, '') " +
	"FROM pg_stat_activity w " +
	"JOIN pg_stat_activity b ON b.pid = ANY(pg_blocking_pids(w.pid)) " +
	"WHERE cardinality(pg_blocking_pids(w.pid)) > 0"

// LockWaits 列出当前锁等待（FR-MON-03）。
func (c *Connector) LockWaits(ctx context.Context) ([]driver.LockWait, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	rows, err := h.QueryContext(ctx, lockWaitSQL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []driver.LockWait{}
	for rows.Next() {
		var lw driver.LockWait
		var wait, block int64
		if err := rows.Scan(&wait, &block, &lw.WaitSeconds, &lw.WaitingSQL, &lw.BlockingSQL); err != nil {
			return nil, err
		}
		lw.WaitingID = strconv.FormatInt(wait, 10)
		lw.BlockingID = strconv.FormatInt(block, 10)
		out = append(out, lw)
	}
	return out, rows.Err()
}
