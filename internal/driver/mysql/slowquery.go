package mysql

import (
	"context"
	"errors"
	"strings"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/maintell/DBRover/internal/driver"
)

// Hint 说明怎么在 MySQL 上打开慢查询统计。
func (c *Connector) Hint() string {
	return "MySQL 的慢查询统计来自 performance_schema：确认 performance_schema = ON（5.6 起默认开），" +
		"并且没有关掉 events_statements_summary_by_digest 的采集。" +
		"如果实例上这张表不存在，说明 performance_schema 被整个关掉了。"
}

// slowQuerySQL 取累计统计。DIGEST 是 binary(32)，直接扫成字符串会得到不可打印
// 字节，所以取 HEX()。TIMER 列的单位是**皮秒**，换算成毫秒在驱动里做——只有它
// 知道这个引擎用什么单位（PG 直接给毫秒）。
const slowQuerySQL = "SELECT HEX(DIGEST), COALESCE(DIGEST_TEXT, ''), COUNT_STAR, SUM_TIMER_WAIT, " +
	"COALESCE(SUM_ROWS_AFFECTED, 0) + COALESCE(SUM_ROWS_SENT, 0) " +
	"FROM performance_schema.events_statements_summary_by_digest " +
	"WHERE DIGEST IS NOT NULL ORDER BY SUM_TIMER_WAIT DESC LIMIT ?"

// QueryStats 读 events_statements_summary_by_digest 的累计统计（FR-MON-04）。
func (c *Connector) QueryStats(ctx context.Context, limit int) ([]driver.QueryStat, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := h.QueryContext(ctx, slowQuerySQL, limit)
	if err != nil {
		// 1146 = 表不存在：performance_schema 被关掉了。这是降级不是失败。
		var me *gomysql.MySQLError
		if errors.As(err, &me) && me.Number == 1146 {
			return nil, driver.ErrSlowQueryUnavailable
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []driver.QueryStat
	for rows.Next() {
		var st driver.QueryStat
		var pico float64
		if err := rows.Scan(&st.Digest, &st.Text, &st.Calls, &pico, &st.Rows); err != nil {
			return nil, err
		}
		st.TotalMS = pico / 1e9
		st.Text = strings.TrimSpace(st.Text)
		out = append(out, st)
	}
	return out, rows.Err()
}
