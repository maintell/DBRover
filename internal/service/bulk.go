package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
)

// ErrBulkNeedsConfirmTable 表示无条件批量修改需要回填表名。
var ErrBulkNeedsConfirmTable = errors.New("无条件批量修改会命中全表，需要回填表名确认")

// ErrBulkNeedsNoRollbackAck 表示影响行数过大，必须显式接受「不生成回滚」。
var ErrBulkNeedsNoRollbackAck = errors.New("影响行数超过回滚生成上限，需显式接受不生成回滚")

// bulkUndoLimit 是生成逐行回滚语句的上限。
// 超过这个量，逐行回滚 SQL 的体积会失控（docs/06 §3.3）。
const bulkUndoLimit = 1000

// bulkSampleLimit 是「前后对比」展示的行数。
const bulkSampleLimit = 5

// BulkUpdateRequest 是一次条件批量修改。
//
// 与变更集的区别：变更集是「我已经看到这几行，改这几行」；
// 批量修改是「按条件改，我事先不知道有多少行」。
type BulkUpdateRequest struct {
	Table model.ObjectRef
	// Set 是要写成的常量值，键为列名。空值表示把列设为 NULL。
	Set map[string]any
	// Filter 为空表示不带 WHERE —— 这是全表写，必须回填表名。
	Filter []FilterCond
	// ConfirmTable 在 Filter 为空时必须等于表名。
	ConfirmTable string
	// AcceptNoRollback 在影响行数超过上限时必须为 true。
	AcceptNoRollback bool
}

// BulkRowDiff 是预览用的前后对比。
type BulkRowDiff struct {
	Key    map[string]any `json:"key"`
	Before map[string]any `json:"before"`
	After  map[string]any `json:"after"`
}

// BulkUpdateResult 是批量修改预案与它的影响面。
type BulkUpdateResult struct {
	Plan     *ChangePlan   `json:"plan"`
	Affected int64         `json:"affected"`
	Samples  []BulkRowDiff `json:"samples,omitempty"`
	// Note 是需要原样展示给用户的说明（例如「本批不生成回滚」）。
	Note string `json:"note,omitempty"`
	// Batched 表示执行时会按主键键集分批推进，而不是一个事务改完。
	// 客户端据此决定是否用带进度的流式执行。
	Batched bool `json:"batched"`
	// BatchSize 是每批行数，Batched 为 false 时无意义。
	BatchSize int `json:"batch_size,omitempty"`
	// Batches 是预计的批次数。
	Batches int64 `json:"batches,omitempty"`
}

// BuildBulkUpdatePlan 生成条件批量修改的预案。
//
// 顺序刻意如此：先校验参数 → 再数影响行数 → 再决定能不能生成回滚 → 最后分级。
// 影响行数必须在生成语句之前拿到，否则「多少个行会被改」只能靠猜。
func (e *Executor) BuildBulkUpdatePlan(ctx context.Context, req BulkUpdateRequest) (*BulkUpdateResult, error) {
	if len(req.Set) == 0 {
		return nil, fmt.Errorf("%w: 至少要设置一列", ErrInvalidDDL)
	}

	td, err := e.conn.Meta().DescribeTable(ctx, req.Table)
	if err != nil {
		return nil, err
	}
	pk := td.PrimaryKeyColumns()
	known := columnCatalog(td)

	table := e.dialect.QuoteQualified(schemaOf(req.Table, e.cfgSchema()), req.Table.Name)

	// 目标列必须存在，值按列类型还原（数值列不该拿字符串去写）。
	setCols := make([]string, 0, len(req.Set))
	args := make([]any, 0, len(req.Set))
	for _, name := range sortedSetKeys(req.Set) {
		ci, ok := known[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			return nil, fmt.Errorf("%w: 列 %q 不存在", ErrInvalidDDL, name)
		}
		if containsFold(pk, ci.name) {
			return nil, fmt.Errorf("%w: 不允许批量修改主键列 %q", ErrInvalidDDL, ci.name)
		}
		setCols = append(setCols, e.dialect.QuoteIdent(ci.name)+" = ?")
		v := req.Set[name]
		if v == nil {
			args = append(args, nil)
			continue
		}
		if f, ok := v.(float64); ok && math.Abs(f) > 1<<53 && ci.typ.Kind == model.TypeInteger {
			return nil, fmt.Errorf("%w（列 %q）", ErrLargeNumberAsJSON, ci.name)
		}
		if s, ok := v.(string); ok {
			args = append(args, coerceKey(s, ci.typ))
		} else {
			args = append(args, v)
		}
	}

	where, wargs, err := e.filterClause(known, req.Filter)
	if err != nil {
		return nil, err
	}

	// 空 WHERE 等于全表写：必须回填表名，防止手滑。
	if len(where) == 0 && !strings.EqualFold(strings.TrimSpace(req.ConfirmTable), req.Table.Name) {
		return nil, ErrBulkNeedsConfirmTable
	}

	affected, err := e.countRows(ctx, table, where, wargs)
	if err != nil {
		return nil, err
	}

	plan := &ChangePlan{
		ID:           "bulk-" + newID(),
		Table:        req.Table,
		CreatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(planTTL),
		AffectedRows: affected,
	}

	sqlText := "UPDATE " + table + " SET " + strings.Join(setCols, ", ")
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	plan.Statements = []PlanStatement{{
		SQL:  sqlText,
		Args: append(append([]any{}, args...), wargs...),
		// 影响行数已精确数过，执行时必须完全一致：中间有人加/删行就整体回滚。
		ExpectRows: int(affected),
	}}

	res := &BulkUpdateResult{Plan: plan, Affected: affected}

	// 回滚：只有在能逐行定位（有主键）且行数在上限内时才生成。
	if len(pk) > 0 && affected > 0 {
		if affected <= bulkUndoLimit {
			undo, samples, err := e.buildBulkUndo(ctx, table, pk, known, req.Filter, where, wargs, req.Set)
			if err == nil {
				plan.Rollback = undo
				res.Samples = samples
			}
		}
	}
	if len(plan.Rollback) == 0 {
		plan.Irreversible = true
		if affected > bulkUndoLimit {
			res.Note = fmt.Sprintf("影响 %d 行，超过逐行回滚生成上限（%d），本次修改不生成回滚；"+
				"如需回滚请先备份这些行。", affected, bulkUndoLimit)
			if !req.AcceptNoRollback {
				return nil, fmt.Errorf("%w（影响 %d 行）", ErrBulkNeedsNoRollbackAck, affected)
			}
		} else if len(pk) == 0 {
			res.Note = "该表没有主键，无法逐行定位，本批不生成回滚。"
		} else {
			res.Note = "没有命中任何行，无需回滚。"
		}
	}

	// 分批：影响行数超过单批上限时按主键键集推进（docs/06 §3.2）。
	//
	// 只有单列主键才启用。多列主键要么得拼出各引擎方言不一的行值比较，
	// 要么让绑定参数个数按主键列数翻倍——都不如老老实实走一个大事务，
	// 代价只是「中途失败整体回滚」，对几万行的量级是可接受的。
	if affected > DefaultBulkBatchSize {
		switch {
		case len(pk) == 1:
			plan.Bulk = &BulkExecPlan{
				Set:       req.Set,
				Filter:    req.Filter,
				PK:        pk[0],
				PKType:    known[strings.ToLower(pk[0])].typ,
				BatchSize: DefaultBulkBatchSize,
			}
			res.Batched = true
			res.BatchSize = DefaultBulkBatchSize
			res.Batches = (affected + int64(DefaultBulkBatchSize) - 1) / int64(DefaultBulkBatchSize)
			res.Note = appendNote(res.Note, fmt.Sprintf(
				"将按主键分 %d 批执行（每批 %d 行），每批各自提交；"+
					"中途失败或中止时，已提交的批不回滚。",
				res.Batches, DefaultBulkBatchSize))
		case len(pk) > 1:
			res.Note = appendNote(res.Note,
				"这张表是复合主键，无法按单列键集分批；将在一个事务里一次改完，中途失败会整体回滚。")
		}
	}

	g := policy.GradeStatement(e.dialect, sqlText, nil)
	plan.Risk = g.Level
	plan.Friction = policy.FrictionFor(e.env, plan.Risk, policy.Modifiers{
		// 批量修改没有逐行预览，不能享受变更集的摩擦折扣。
		StatementCount:     1,
		RowsAffected:       affected,
		ReadOnlyConnection: !e.mode.AllowsWrite(),
	})
	return res, nil
}

// appendNote 把一句说明接到已有说明后面，空说明时不留下前导空格。
func appendNote(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + " " + add
}

// sortedSetKeys 让 SET 子句的列顺序稳定，便于测试与审计比对。
func sortedSetKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// countRows 精确数出会被改到的行数。
func (e *Executor) countRows(ctx context.Context, table string, where []string, args []any) (int64, error) {
	q := "SELECT COUNT(*) FROM " + table
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = sess.Close() }()

	cur, err := sess.Query(ctx, q, args, driver.QueryOptions{})
	if err != nil {
		return 0, err
	}
	rows, _, err := cur.Next(ctx)
	_ = cur.Close()
	if err != nil || len(rows) != 1 || len(rows[0]) == 0 {
		return 0, fmt.Errorf("统计影响行数失败")
	}
	n, ok := toInt64(rows[0][0])
	if !ok {
		return 0, fmt.Errorf("统计影响行数失败：返回值不是整数")
	}
	return n, nil
}

// buildBulkUndo 按主键逐行生成回滚语句，同时给出前后对比样本。
//
// 只取会被改到的列与主键，不读整行：宽表上少传一半数据。
func (e *Executor) buildBulkUndo(
	ctx context.Context,
	table string,
	pk []string,
	known map[string]columnRef,
	filter []FilterCond,
	where []string,
	args []any,
	set map[string]any,
) ([]PlanStatement, []BulkRowDiff, error) {

	selectCols := make([]string, 0, len(pk)+len(set))
	for _, c := range pk {
		selectCols = append(selectCols, e.dialect.QuoteIdent(c))
	}
	setNames := make([]string, 0, len(set))
	for _, name := range sortedSetKeys(set) {
		ci := known[strings.ToLower(strings.TrimSpace(name))]
		setNames = append(setNames, ci.name)
		selectCols = append(selectCols, e.dialect.QuoteIdent(ci.name))
	}

	q := "SELECT " + strings.Join(selectCols, ", ") + " FROM " + table
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY " + strings.Join(quoteAllIn(e, pk), ", ")

	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = sess.Close() }()

	cur, err := sess.Query(ctx, q, args, driver.QueryOptions{FetchSize: bulkUndoLimit + 1})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = cur.Close() }()

	var all [][]any
	for len(all) <= bulkUndoLimit {
		batch, more, err := cur.Next(ctx)
		if err != nil {
			return nil, nil, err
		}
		all = append(all, batch...)
		if !more {
			break
		}
	}
	if len(all) > bulkUndoLimit {
		// 统计之后又长出了行：宁可放弃回滚，也不生成半套撤销语句。
		return nil, nil, errors.New("行数超过回滚生成上限")
	}

	undo := make([]PlanStatement, 0, len(all))
	samples := make([]BulkRowDiff, 0, bulkSampleLimit)
	for _, row := range all {
		key := map[string]any{}
		before := map[string]any{}
		after := map[string]any{}
		assigns := make([]string, 0, len(setNames))
		backArgs := make([]any, 0, len(setNames)+len(pk))

		for i, c := range pk {
			key[c] = row[i]
		}
		offset := len(pk)
		for j, name := range setNames {
			oldVal := row[offset+j]
			before[name] = oldVal
			after[name] = set[name]
			assigns = append(assigns, e.dialect.QuoteIdent(name)+" = ?")
			backArgs = append(backArgs, oldVal)
		}
		backArgs = append(backArgs, keyArgs(pk, key)...)

		undo = append(undo, PlanStatement{
			SQL:        "UPDATE " + table + " SET " + strings.Join(assigns, ", ") + " WHERE " + pkWhere(e, pk),
			Args:       backArgs,
			ExpectRows: 1,
		})
		if len(samples) < bulkSampleLimit {
			samples = append(samples, BulkRowDiff{Key: key, Before: before, After: after})
		}
	}
	return undo, samples, nil
}

func quoteAllIn(e *Executor, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = e.dialect.QuoteIdent(n)
	}
	return out
}
