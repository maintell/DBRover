package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

// MigrateExecOptions 控制数据搬运（ADR-0011）。
type MigrateExecOptions struct {
	// BatchSize 是每批行数，默认 1000（与批量修改一致）。
	BatchSize int
	// MaxRows > 0 时只搬这么多行就停（演练/测试用）。
	MaxRows int
}

// MigrateExecReport 是一次数据搬运的结果。
type MigrateExecReport struct {
	Table   string `json:"table"`
	Rows    int64  `json:"rows"`
	Batches int    `json:"batches"`
	// KeysetOK 表示这张表有主键，具备「键集推进」的前提。
	// **注意**：检查点落盘与断点续跑尚未实现（ADR-0011 的验证计划 1 未做），
	// 所以它现在只说明「这张表将来可以续跑」，不代表本次已经能中断续传。
	KeysetOK bool `json:"keyset_ok"`
	// Verified 为 false 表示没有做内容校验（无主键时顺序不可比）。
	Verified     bool   `json:"verified"`
	SourceDigest string `json:"source_digest,omitempty"`
	TargetDigest string `json:"target_digest,omitempty"`
	Note         string `json:"note,omitempty"`
}

// ErrMigrateVerify 表示搬完之后两端内容对不上。
var ErrMigrateVerify = errors.New("迁移校验失败：源与目标内容不一致")

// MigrateTable 把一张表的数据从 src 搬到 dst（目标结构必须已存在）。
//
// 三条口径来自 ADR-0011：按主键键集推进（并发写入下不跳行不重复）、不做 upsert
// （重复主键直接报错）、搬完用「行数 + 按主键排序的全量哈希」校验。
func MigrateTable(ctx context.Context, src, dst *Executor, table, targetTable string, opt MigrateExecOptions) (*MigrateExecReport, error) {
	if src == nil || dst == nil {
		return nil, errors.New("迁移：缺少源或目标执行器")
	}
	if opt.BatchSize <= 0 {
		opt.BatchSize = 1000
	}
	if targetTable == "" {
		targetTable = table
	}

	srcDetail, err := src.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: table})
	if err != nil {
		return nil, fmt.Errorf("读源表结构: %w", err)
	}
	dstDetail, err := dst.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: targetTable})
	if err != nil {
		return nil, fmt.Errorf("读目标表结构（请先执行建表预案）: %w", err)
	}

	cols, err := migrateColumns(srcDetail, dstDetail)
	if err != nil {
		return nil, err
	}
	pk := srcDetail.PrimaryKeyColumns()
	rep := &MigrateExecReport{Table: table, KeysetOK: len(pk) > 0, Verified: len(pk) > 0}
	if len(pk) == 0 {
		rep.Note = "该表没有主键：不可中断续跑，也不做内容校验（顺序不可比）；只核对行数"
	}

	srcSess, err := src.conn.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = srcSess.Close() }()

	quotedTable := src.dialect.QuoteQualified("", table)
	selectCols := make([]string, len(cols))
	for i, c := range cols {
		selectCols[i] = src.dialect.QuoteIdent(c)
	}
	digest := &tableDigest{}
	var last []any

	for {
		if opt.MaxRows > 0 && rep.Rows >= int64(opt.MaxRows) {
			break
		}
		limit := opt.BatchSize
		if opt.MaxRows > 0 && int64(limit) > int64(opt.MaxRows)-rep.Rows {
			limit = int(opt.MaxRows) - int(rep.Rows)
		}
		q, args := buildMigrateSelect(src, quotedTable, selectCols, pk, last, limit)
		rows, err := fetchRows(ctx, srcSess, q, args, limit)
		if err != nil {
			return nil, fmt.Errorf("读源数据: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		if err := insertRows(ctx, dst, targetTable, cols, rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			if rep.Verified {
				digest.Add(r)
			}
		}
		rep.Rows += int64(len(rows))
		rep.Batches++
		if len(pk) > 0 {
			last = pkValues(pk, cols, rows[len(rows)-1])
		}
		if len(rows) < limit {
			break
		}
	}

	if rep.Verified {
		rep.SourceDigest = digest.Hex()
		targetHex, targetRows, err := migrationDigest(ctx, dst, targetTable, cols, pk)
		if err != nil {
			return rep, fmt.Errorf("校验目标数据: %w", err)
		}
		rep.TargetDigest = targetHex
		if err := compareMigrateDigests(table, rep.SourceDigest, rep.Rows, targetHex, targetRows); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// migrateColumns 取两侧同名列，按源顺序输出；源有而目标没有的列直接报错。
func migrateColumns(src, dst *model.TableDetail) ([]string, error) {
	have := map[string]bool{}
	for _, c := range dst.Columns {
		have[strings.ToLower(c.Name)] = true
	}
	var out []string
	var missing []string
	for _, c := range src.Columns {
		if !have[strings.ToLower(c.Name)] {
			missing = append(missing, c.Name)
			continue
		}
		out = append(out, c.Name)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("迁移：目标表缺少列 %s（先执行建表预案）", strings.Join(missing, "、"))
	}
	return out, nil
}

// buildMigrateSelect 拼键集分页的 SELECT。
func buildMigrateSelect(ex *Executor, table string, cols, pk []string, last []any, limit int) (string, []any) {
	q := "SELECT " + strings.Join(cols, ", ") + " FROM " + table
	var args []any
	if len(pk) > 0 && len(last) == len(pk) {
		quoted := make([]string, len(pk))
		ph := make([]string, len(pk))
		for i, c := range pk {
			quoted[i] = ex.dialect.QuoteIdent(c)
			ph[i] = "?"
			args = append(args, last[i])
		}
		q += " WHERE (" + strings.Join(quoted, ", ") + ") > (" + strings.Join(ph, ", ") + ")"
	}
	if len(pk) > 0 {
		q += " ORDER BY " + strings.Join(quotedOr(pk, ex), ", ")
	}
	q += fmt.Sprintf(" LIMIT %d", limit)
	return q, args
}

func quotedOr(names []string, ex *Executor) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = ex.dialect.QuoteIdent(n)
	}
	return out
}

// fetchRows 读一批（游标可能分多次返回）。
func fetchRows(ctx context.Context, sess driver.Session, q string, args []any, limit int) ([][]any, error) {
	cur, err := sess.Query(ctx, q, args, driver.QueryOptions{MaxRows: limit, FetchSize: limit})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close() }()
	var out [][]any
	for {
		rows, more, err := cur.Next(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if !more || len(out) >= limit {
			break
		}
	}
	return out, nil
}

// insertRows 用多行 INSERT 写一批，走目标端的执行门（审批/审计/策略照旧）。
func insertRows(ctx context.Context, dst *Executor, table string, cols []string, rows [][]any) error {
	quotedTable := dst.dialect.QuoteQualified("", table)
	quotedCols := quotedOr(cols, dst)
	oneRow := "(" + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + ")"
	parts := make([]string, len(rows))
	args := make([]any, 0, len(rows)*len(cols))
	for i, r := range rows {
		parts[i] = oneRow
		args = append(args, r...)
	}
	q := "INSERT INTO " + quotedTable + " (" + strings.Join(quotedCols, ", ") + ") VALUES " + strings.Join(parts, ", ")
	out, err := dst.Execute(ctx, Request{SQL: q, Args: args, Approval: true, Actor: "human:migrate"})
	if err != nil {
		return fmt.Errorf("写目标表: %w", err)
	}
	if out.Err != nil {
		return fmt.Errorf("写目标表: %s（提示：目标表已有数据时请先清空，或改用续跑）", out.Err.Message)
	}
	return nil
}

// pkValues 从一行里按主键列取出键值。
func pkValues(pk, cols []string, row []any) []any {
	idx := map[string]int{}
	for i, c := range cols {
		idx[strings.ToLower(c)] = i
	}
	out := make([]any, len(pk))
	for i, c := range pk {
		if j, ok := idx[strings.ToLower(c)]; ok {
			out[i] = row[j]
		}
	}
	return out
}

// migrationDigest 按主键顺序读一张表并算摘要。
func migrationDigest(ctx context.Context, ex *Executor, table string, cols, pk []string) (string, int64, error) {
	sess, err := ex.conn.Acquire(ctx)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = sess.Close() }()
	q := "SELECT " + strings.Join(quotedOr(cols, ex), ", ") + " FROM " + ex.dialect.QuoteQualified("", table)
	if len(pk) > 0 {
		q += " ORDER BY " + strings.Join(quotedOr(pk, ex), ", ")
	}
	cur, err := sess.Query(ctx, q, nil, driver.QueryOptions{MaxRows: 0, FetchSize: 1000})
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = cur.Close() }()
	d := &tableDigest{}
	for {
		rows, more, err := cur.Next(ctx)
		if err != nil {
			return "", 0, err
		}
		for _, r := range rows {
			d.Add(r)
		}
		if !more {
			break
		}
	}
	return d.Hex(), d.Rows(), nil
}

// VerifyMigratedTable 只做校验，不搬数据：两端行数与摘要必须一致。
//
// 单独一个函数是因为「演练报告」需要在搬完之后随时复查（例如用户改过目标库）。
func VerifyMigratedTable(ctx context.Context, src, dst *Executor, table, targetTable string) (*MigrateExecReport, error) {
	if targetTable == "" {
		targetTable = table
	}
	srcDetail, err := src.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: table})
	if err != nil {
		return nil, err
	}
	dstDetail, err := dst.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: targetTable})
	if err != nil {
		return nil, err
	}
	cols, err := migrateColumns(srcDetail, dstDetail)
	if err != nil {
		return nil, err
	}
	pk := srcDetail.PrimaryKeyColumns()
	rep := &MigrateExecReport{Table: table, KeysetOK: len(pk) > 0, Verified: len(pk) > 0}
	if len(pk) == 0 {
		rep.Note = "该表没有主键：只核对行数，不做内容校验（顺序不可比）"
		srcN, err := countRows(ctx, src, table)
		if err != nil {
			return nil, err
		}
		dstN, err := countRows(ctx, dst, targetTable)
		if err != nil {
			return nil, err
		}
		rep.Rows, rep.TargetDigest = srcN, ""
		if srcN != dstN {
			return rep, fmt.Errorf("%w：源 %d 行，目标 %d 行", ErrMigrateVerify, srcN, dstN)
		}
		return rep, nil
	}
	srcHex, srcRows, err := migrationDigest(ctx, src, table, cols, pk)
	if err != nil {
		return nil, err
	}
	dstHex, dstRows, err := migrationDigest(ctx, dst, targetTable, cols, pk)
	if err != nil {
		return nil, err
	}
	rep.Rows, rep.SourceDigest, rep.TargetDigest = srcRows, srcHex, dstHex
	if err := compareMigrateDigests(table, srcHex, srcRows, dstHex, dstRows); err != nil {
		return rep, err
	}
	return rep, nil
}

// compareMigrateDigests 是唯一的摘要比对点。
//
// 抽出来是因为之前搬运动作与独立复查各写了一份，改坏其中一处测试仍然全绿——
// 校验这种「只在出错时才有价值」的代码，绝不能有两份。
func compareMigrateDigests(table, srcHex string, srcRows int64, dstHex string, dstRows int64) error {
	if srcRows != dstRows {
		return fmt.Errorf("%w：源 %d 行，目标 %d 行", ErrMigrateVerify, srcRows, dstRows)
	}
	if srcHex != dstHex {
		return fmt.Errorf("%w：%s 的摘要不一致（源 %s，目标 %s）", ErrMigrateVerify, table, srcHex, dstHex)
	}
	return nil
}

// countRows 数一张表的行数。
func countRows(ctx context.Context, ex *Executor, table string) (int64, error) {
	sess, err := ex.conn.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = sess.Close() }()
	cur, err := sess.Query(ctx, "SELECT COUNT(*) FROM "+ex.dialect.QuoteQualified("", table), nil,
		driver.QueryOptions{MaxRows: 1})
	if err != nil {
		return 0, err
	}
	defer func() { _ = cur.Close() }()
	rows, _, err := cur.Next(ctx)
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		return 0, fmt.Errorf("统计 %s 行数失败: %v", table, err)
	}
	return countValue(rows[0][0]), nil
}
