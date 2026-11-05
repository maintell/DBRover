package pg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/maintell/DBRover/internal/driver"
)

// Hint 说明怎么在 PG 上打开慢查询统计。
//
// 三步缺一不可：扩展要 preload（否则 CREATE EXTENSION 直接报错）、要重启
// （preload 只读启动参数）、最后才是建扩展。
func (c *Connector) Hint() string {
	return "PostgreSQL 的慢查询统计要靠 pg_stat_statements：" +
		"1) 在 postgresql.conf 设 shared_preload_libraries = 'pg_stat_statements'；" +
		"2) 重启实例（这个参数只在启动时读）；" +
		"3) 在目标库执行 CREATE EXTENSION pg_stat_statements;"
}

// slowQuerySQL 按总耗时倒序取累计统计。
//
// 用普通字符串而不是反引号 raw string：这段语句没有单引号之外的转义需求，
// 而用代码生成源码时 raw string 的边界很容易被写进产物（真机上表现为
// syntax error at or near ""）。
const slowQuerySQL = "SELECT queryid::text, query, calls, total_exec_time, COALESCE(rows, -1) " +
	"FROM pg_stat_statements ORDER BY total_exec_time DESC LIMIT $1"

// QueryStats 读 pg_stat_statements 的累计统计（FR-MON-04）。
//
// 扩展没装时返回 driver.ErrSlowQueryUnavailable：它是**降级**不是失败，上层会翻成
// 一句「怎么打开」而不是 5xx。
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
		// 42P01 = undefined_table：扩展没建，或者没 preload 导致建不出来。
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "42P01" {
			return nil, driver.ErrSlowQueryUnavailable
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []driver.QueryStat
	for rows.Next() {
		var st driver.QueryStat
		if err := rows.Scan(&st.Digest, &st.Text, &st.Calls, &st.TotalMS, &st.Rows); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
