package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

// ErrDuplicateRows 表示目标列上存在重复值，不能建唯一索引。
//
// 这类失败必须拦在生成预案时：直接建唯一索引，引擎会报错（PG/MySQL 都会），
// 但在大表上这一下可能要扫全表才发现，且错误信息里不带样本，用户还得自己找。
var ErrDuplicateRows = errors.New("目标列上存在重复值，无法创建唯一索引")

// IndexSpec 描述一个索引。
type IndexSpec struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique"`
}

// normalizeIndex 校验索引定义，并把列名还原成表里的真实写法。
func normalizeIndex(spec IndexSpec, td *model.TableDetail) (IndexSpec, error) {
	name, err := normalizeColumnName(spec.Name)
	if err != nil {
		// 索引名与列名受同样的约束：它是标识符，不能带引号或分号。
		return spec, fmt.Errorf("%w: 索引名 %q 不合法", ErrInvalidDDL, spec.Name)
	}
	if len(spec.Columns) == 0 {
		return spec, fmt.Errorf("%w: 索引至少要有一列", ErrInvalidDDL)
	}

	seen := map[string]bool{}
	cols := make([]string, 0, len(spec.Columns))
	for _, c := range spec.Columns {
		col, ok := findColumn(td, strings.TrimSpace(c))
		if !ok {
			return spec, fmt.Errorf("%w: 索引列 %q 不存在", ErrInvalidDDL, c)
		}
		key := strings.ToLower(col.Name)
		if seen[key] {
			return spec, fmt.Errorf("%w: 索引列 %q 重复", ErrInvalidDDL, col.Name)
		}
		seen[key] = true
		cols = append(cols, col.Name)
	}
	return IndexSpec{Name: name, Columns: cols, Unique: spec.Unique}, nil
}

// probeDuplicates 数出「按目标列分组后行数 > 1」的组数，并取几组样本。
//
// 语义细节：唯一索引允许多个 NULL（PG/MySQL 都把 NULL 视为互不相等），
// 因此探测时排除任一列为 NULL 的行，否则会把合法的数据判成重复。
func (e *Executor) probeDuplicates(ctx context.Context, table string, columns []string) (int64, []string, error) {
	quoted := make([]string, len(columns))
	notNull := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = e.dialect.QuoteIdent(c)
		notNull[i] = quoted[i] + " IS NOT NULL"
	}
	groupBy := strings.Join(quoted, ", ")
	where := " WHERE " + strings.Join(notNull, " AND ")

	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = sess.Close() }()

	countQ := "SELECT COUNT(*) FROM (SELECT 1 FROM " + table + where +
		" GROUP BY " + groupBy + " HAVING COUNT(*) > 1) AS dup"
	cur, err := sess.Query(ctx, countQ, nil, driver.QueryOptions{})
	if err != nil {
		return 0, nil, err
	}
	rows, _, err := cur.Next(ctx)
	_ = cur.Close()
	if err != nil || len(rows) != 1 || len(rows[0]) == 0 {
		return 0, nil, fmt.Errorf("统计重复值失败")
	}
	bad, ok := toInt64(rows[0][0])
	if !ok || bad == 0 {
		return 0, nil, nil
	}

	samples := make([]string, 0, 3)
	sampleQ := "SELECT " + groupBy + " FROM " + table + where +
		" GROUP BY " + groupBy + " HAVING COUNT(*) > 1 LIMIT 3"
	scur, err := sess.Query(ctx, sampleQ, nil, driver.QueryOptions{FetchSize: 3})
	if err == nil {
		srows, _, serr := scur.Next(ctx)
		_ = scur.Close()
		if serr == nil {
			for i, r := range srows {
				if i >= 3 {
					break
				}
				parts := make([]string, 0, len(r))
				for _, v := range r {
					parts = append(parts, fmt.Sprint(v))
				}
				samples = append(samples, "("+strings.Join(parts, ", ")+")")
			}
		}
	}
	return bad, samples, nil
}
