package mysql

import (
	"context"
	"strconv"

	"github.com/maintell/DBRover/internal/driver"
)

// lockWaitSQL 取 InnoDB 的锁等待边。
//
// 三个 innodb_trx 别名不是冗余：requesting 给出等待方，blocking 给出持有方，r 单独
// 取来算等待时长（trx_wait_started 在 requesting 那行上）。只用两个别名会把
// 「等了多久」算成整个事务的时长，那和「这条语句被挡了多久」不是一回事。
const lockWaitSQL = "SELECT wt.trx_mysql_thread_id, bt.trx_mysql_thread_id, " +
	"COALESCE(TIMESTAMPDIFF(SECOND, r.trx_wait_started, NOW()), -1), " +
	"COALESCE(wt.trx_query, ''), COALESCE(bt.trx_query, '') " +
	"FROM information_schema.innodb_lock_waits w " +
	"JOIN information_schema.innodb_trx r ON r.trx_id = w.requesting_trx_id " +
	"JOIN information_schema.innodb_trx wt ON wt.trx_id = w.requesting_trx_id " +
	"JOIN information_schema.innodb_trx bt ON bt.trx_id = w.blocking_trx_id"

// LockWaits 列出当前锁等待（FR-MON-03）。
//
// 没有等待时 innodb_lock_waits 是空表，所以空切片就是「真没有阻塞」，不是失败。
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
