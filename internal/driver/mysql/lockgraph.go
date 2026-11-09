package mysql

import (
	"context"
	"strconv"
	"strings"

	"github.com/maintell/DBRover/internal/driver"
)

// lockWaitSQL57 取 MySQL 5.7 / MariaDB 的 InnoDB 锁等待边。
//
// 三个 innodb_trx 别名不是冗余：requesting 给出等待方，blocking 给出持有方，r 单独
// 取来算等待时长（trx_wait_started 在 requesting 那行上）。只用两个别名会把
// 「等了多久」算成整个事务的时长，那和「这条语句被挡了多久」不是一回事。
const lockWaitSQL57 = "SELECT wt.trx_mysql_thread_id, bt.trx_mysql_thread_id, " +
	"COALESCE(TIMESTAMPDIFF(SECOND, r.trx_wait_started, NOW()), -1), " +
	"COALESCE(wt.trx_query, ''), COALESCE(bt.trx_query, '') " +
	"FROM information_schema.innodb_lock_waits w " +
	"JOIN information_schema.innodb_trx r ON r.trx_id = w.requesting_trx_id " +
	"JOIN information_schema.innodb_trx wt ON wt.trx_id = w.requesting_trx_id " +
	"JOIN information_schema.innodb_trx bt ON bt.trx_id = w.blocking_trx_id"

// lockWaitSQL80 取 MySQL 8.0+ 的锁等待边。
//
// 8.0 把 InnoDB 锁等待从 information_schema.innodb_lock_waits 迁到了
// performance_schema.data_lock_waits，前者在 8.0 上已不存在（实测 8.0.39 报
// 「Unknown table 'INNODB_LOCK_WAITS' in information_schema」）。data_lock_waits
// 只给 THREAD_ID，必须经 performance_schema.threads 映射回 PROCESSLIST_ID——界面与
// 会话/终止功能都按 PROCESSLIST_ID 认会话。等待时长取 PROCESSLIST_TIME（秒），
// 语句取 PROCESSLIST_INFO。空表即「当前没有锁等待」，是健康状态。
const lockWaitSQL80 = "SELECT wt.PROCESSLIST_ID, bt.PROCESSLIST_ID, " +
	"COALESCE(wt.PROCESSLIST_TIME, -1), " +
	"COALESCE(wt.PROCESSLIST_INFO, ''), COALESCE(bt.PROCESSLIST_INFO, '') " +
	"FROM performance_schema.data_lock_waits w " +
	"JOIN performance_schema.threads wt ON wt.THREAD_ID = w.REQUESTING_THREAD_ID " +
	"JOIN performance_schema.threads bt ON bt.THREAD_ID = w.BLOCKING_THREAD_ID"

// pickLockWaitSQL 按主版本号选锁等待查询：8 及以上走 performance_schema，其余走
// information_schema。抽成纯函数以便无真机时也能单测分派逻辑（见 lockgraph_test.go）。
func pickLockWaitSQL(major int) string {
	if major >= 8 {
		return lockWaitSQL80
	}
	return lockWaitSQL57
}

// parseMajorVersion 从 SELECT VERSION() 的返回里取主版本号。版本串可能带后缀
// （8.0.39-log、10.11.2-MariaDB），取第一段数字；解析不了返回 0。
func parseMajorVersion(v string) int {
	head := v
	if i := strings.IndexByte(head, '.'); i >= 0 {
		head = head[:i]
	} else if i := strings.IndexAny(head, "- "); i >= 0 {
		head = head[:i]
	}
	n, err := strconv.Atoi(strings.TrimSpace(head))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// serverMajor 探测服务器主版本。探测失败返回 0，调用方据此降级到 5.7 路径——
// 不能因为一次轻量的版本查询失败就让整个锁等待功能不可用。
func (c *Connector) serverMajor(ctx context.Context) int {
	h, err := c.handle()
	if err != nil {
		return 0
	}
	var v string
	if err := h.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
		return 0
	}
	return parseMajorVersion(v)
}

// LockWaits 列出当前锁等待（FR-MON-03）。
//
// 没有等待时锁等待表是空表，所以空切片就是「真没有阻塞」，不是失败。MySQL 8.0
// 起改用 performance_schema.data_lock_waits，见 pickLockWaitSQL。
func (c *Connector) LockWaits(ctx context.Context) ([]driver.LockWait, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	rows, err := h.QueryContext(ctx, pickLockWaitSQL(c.serverMajor(ctx)))
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
