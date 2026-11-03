package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

// FKPrecheckRequest 是加外键前的预检请求。
type FKPrecheckRequest struct {
	Child      model.ObjectRef
	Parent     model.ObjectRef
	Columns    []string // 子表列
	RefColumns []string // 父表列，与 Columns 一一对应
}

// FKPrecheck 是预检结论。
type FKPrecheck struct {
	// Orphans 是「子表有值但父表找不到对应行」的行数。
	Orphans int64 `json:"orphans"`
	// Samples 是最多 5 条孤儿行的键值，供人定位。
	Samples []string `json:"samples,omitempty"`
	// ParentUnique 表示父表这几列是否唯一（主键或唯一索引）。
	// 不唯一时外键能建但语义是错的：一行子记录会对应多行父记录。
	ParentUnique bool `json:"parent_unique"`
	// Blocked 为 true 表示当前数据下不该加这个外键。
	Blocked bool `json:"blocked"`
	// Reason 是给用户看的结论。
	Reason string `json:"reason"`
}

// PrecheckForeignKey 检查「现在加这个外键会不会踩到既有数据」。
//
// 只读，不改任何东西。之所以把它单独做出来：
//   - 直接加外键，引擎会在建约束时扫全表才发现孤儿行，报错里不带样本，用户无从下手；
//   - 大表上一次失败的 DDL 可能持有锁很久。
func (e *Executor) PrecheckForeignKey(ctx context.Context, req FKPrecheckRequest) (*FKPrecheck, error) {
	if len(req.Columns) == 0 {
		return nil, fmt.Errorf("%w: 至少要指定一列", ErrInvalidDDL)
	}
	if len(req.Columns) != len(req.RefColumns) {
		return nil, fmt.Errorf("%w: 子表列与父表列数量不一致（%d vs %d）",
			ErrInvalidDDL, len(req.Columns), len(req.RefColumns))
	}

	childTD, err := e.conn.Meta().DescribeTable(ctx, req.Child)
	if err != nil {
		return nil, err
	}
	parentTD, err := e.conn.Meta().DescribeTable(ctx, req.Parent)
	if err != nil {
		return nil, err
	}

	childCols := make([]string, len(req.Columns))
	for i, c := range req.Columns {
		col, ok := findColumn(childTD, c)
		if !ok {
			return nil, fmt.Errorf("%w: 子表没有列 %q", ErrInvalidDDL, c)
		}
		childCols[i] = col.Name
	}
	parentCols := make([]string, len(req.RefColumns))
	for i, c := range req.RefColumns {
		col, ok := findColumn(parentTD, c)
		if !ok {
			return nil, fmt.Errorf("%w: 父表没有列 %q", ErrInvalidDDL, c)
		}
		parentCols[i] = col.Name
	}

	child := e.dialect.QuoteQualified(schemaOf(req.Child, e.cfgSchema()), req.Child.Name)
	parent := e.dialect.QuoteQualified(schemaOf(req.Parent, e.cfgSchema()), req.Parent.Name)

	// 任一子列是 NULL 的行不参与外键检查：NULL 表示「没有引用」，不是孤儿。
	notNull := make([]string, len(childCols))
	for i, c := range childCols {
		notNull[i] = "c." + e.dialect.QuoteIdent(c) + " IS NOT NULL"
	}
	pairs := make([]string, len(parentCols))
	childRefs := make([]string, len(childCols))
	for i := range parentCols {
		pairs[i] = "p." + e.dialect.QuoteIdent(parentCols[i]) + " = c." + e.dialect.QuoteIdent(childCols[i])
		childRefs[i] = "c." + e.dialect.QuoteIdent(childCols[i])
	}

	orphanCond := "(" + strings.Join(notNull, " AND ") + ") AND NOT EXISTS (SELECT 1 FROM " +
		parent + " AS p WHERE " + strings.Join(pairs, " AND ") + ")"

	out := &FKPrecheck{ParentUnique: uniqueOnColumns(parentTD, parentCols)}

	countQ := "SELECT COUNT(*) FROM " + child + " AS c WHERE " + orphanCond
	orphans, err := e.scalarInt(ctx, countQ)
	if err != nil {
		return nil, err
	}
	out.Orphans = orphans

	if orphans > 0 {
		sampleQ := "SELECT " + strings.Join(childRefs, ", ") + " FROM " + child + " AS c WHERE " +
			orphanCond + " LIMIT 5"
		out.Samples = e.sampleRows(ctx, sampleQ, len(childCols))
	}

	switch {
	case orphans > 0 && !out.ParentUnique:
		out.Blocked = true
		out.Reason = fmt.Sprintf("父表这几列不唯一，且子表有 %d 行在父表找不到对应行", orphans)
	case orphans > 0:
		out.Blocked = true
		out.Reason = fmt.Sprintf("子表有 %d 行在父表找不到对应行（孤儿数据），加外键会失败", orphans)
	case !out.ParentUnique:
		// 能建，但语义可疑：一行子记录会对应多行父记录。
		out.Blocked = false
		out.Reason = "父表这几列不唯一：约束可以建，但一行子记录会对应多行父记录，请确认这是你要的语义"
	default:
		out.Reason = "没有孤儿数据，父表列唯一，可以加外键"
	}
	return out, nil
}

// uniqueOnColumns 判断这些列上是否有主键或唯一索引（列集合必须完全一致）。
// 只有「列集合相同」才算——(a) 的单列唯一索引不能保证 (a,b) 唯一。
func uniqueOnColumns(td *model.TableDetail, cols []string) bool {
	want := make(map[string]bool, len(cols))
	for _, c := range cols {
		want[strings.ToLower(c)] = true
	}
	if pk := td.PrimaryKeyColumns(); len(pk) == len(cols) {
		got := make(map[string]bool, len(pk))
		for _, c := range pk {
			got[strings.ToLower(c)] = true
		}
		if sameSet(want, got) {
			return true
		}
	}
	for _, ix := range td.Indexes {
		if !ix.Unique || len(ix.Columns) != len(cols) {
			continue
		}
		got := make(map[string]bool, len(ix.Columns))
		for _, c := range ix.Columns {
			got[strings.ToLower(c)] = true
		}
		if sameSet(want, got) {
			return true
		}
	}
	return false
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// scalarInt 执行一条返回单个整数的查询。
func (e *Executor) scalarInt(ctx context.Context, q string) (int64, error) {
	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = sess.Close() }()

	cur, err := sess.Query(ctx, q, nil, driver.QueryOptions{})
	if err != nil {
		return 0, err
	}
	rows, _, err := cur.Next(ctx)
	_ = cur.Close()
	if err != nil || len(rows) != 1 || len(rows[0]) == 0 {
		return 0, fmt.Errorf("查询失败")
	}
	n, ok := toInt64(rows[0][0])
	if !ok {
		return 0, fmt.Errorf("返回值不是整数")
	}
	return n, nil
}

// sampleRows 取最多 5 行样本并拼成可读文本。
func (e *Executor) sampleRows(ctx context.Context, q string, width int) []string {
	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = sess.Close() }()

	cur, err := sess.Query(ctx, q, nil, driver.QueryOptions{FetchSize: 5})
	if err != nil {
		return nil
	}
	defer func() { _ = cur.Close() }()

	rows, _, err := cur.Next(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, 5)
	for i, r := range rows {
		if i >= 5 || len(r) < width {
			break
		}
		parts := make([]string, 0, width)
		for _, v := range r[:width] {
			parts = append(parts, fmt.Sprint(v))
		}
		out = append(out, "("+strings.Join(parts, ", ")+")")
	}
	return out
}
