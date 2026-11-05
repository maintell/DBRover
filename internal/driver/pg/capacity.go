package pg

import (
	"context"

	"github.com/maintell/DBRover/internal/driver"
)

// tableSizeSQL 取当前库里最大的几张表。
//
// 用普通字符串而不是反引号 raw string：这段语句没有需要 raw string 的转义，
// 而用代码生成源码时 raw string 的边界很容易被写进产物。
const tableSizeSQL = "SELECT n.nspname, c.relname, " +
	"pg_table_size(c.oid)::bigint, pg_indexes_size(c.oid)::bigint, " +
	"pg_total_relation_size(c.oid)::bigint, " +
	"COALESCE((SELECT s.n_live_tup FROM pg_stat_user_tables s WHERE s.relid = c.oid), -1)::bigint " +
	"FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace " +
	"WHERE c.relkind IN ('r','m') AND n.nspname NOT IN ('pg_catalog','information_schema') " +
	"AND n.nspname NOT LIKE 'pg_toast%' " +
	"ORDER BY pg_total_relation_size(c.oid) DESC LIMIT $1"

// TableSizes 读表体量（FR-MON-06）。
//
// 只查目录与统计视图，不扫表；pg_table_size / pg_indexes_size 都是 O(1) 的目录计算。
func (c *Connector) TableSizes(ctx context.Context, limit int) ([]driver.TableSize, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 10
	}
	rows, err := h.QueryContext(ctx, tableSizeSQL, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []driver.TableSize
	for rows.Next() {
		var t driver.TableSize
		if err := rows.Scan(&t.Schema, &t.Name, &t.DataBytes, &t.IndexBytes, &t.TotalBytes, &t.Rows); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
