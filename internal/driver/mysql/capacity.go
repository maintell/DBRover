package mysql

import (
	"context"

	"github.com/maintell/DBRover/internal/driver"
)

// capacitySQL 取用户库里最大的几张表。
//
// 排除系统库：它们总是排在前几名，把真正的业务表挤掉。TABLE_ROWS 在 InnoDB 上是
// 估算值，所以原样透出、由界面说明是估算。
const capacitySQL = "SELECT TABLE_SCHEMA, TABLE_NAME, " +
	"COALESCE(DATA_LENGTH, 0), COALESCE(INDEX_LENGTH, 0), " +
	"COALESCE(DATA_LENGTH, 0) + COALESCE(INDEX_LENGTH, 0), COALESCE(TABLE_ROWS, -1) " +
	"FROM information_schema.TABLES " +
	"WHERE TABLE_TYPE = 'BASE TABLE' " +
	"AND TABLE_SCHEMA NOT IN ('mysql','information_schema','performance_schema','sys') " +
	"ORDER BY COALESCE(DATA_LENGTH, 0) + COALESCE(INDEX_LENGTH, 0) DESC LIMIT ?"

// TableSizes 读表体量（FR-MON-06）。
func (c *Connector) TableSizes(ctx context.Context, limit int) ([]driver.TableSize, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 10
	}
	rows, err := h.QueryContext(ctx, capacitySQL, limit)
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
