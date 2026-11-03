package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

// BulkExecPlan 是分批执行批量修改所需的全部信息。
//
// 它挂在 ChangePlan 上但不出网（json:"-"）：客户端只需要知道「影响多少行、会执行什么」，
// 分批的推进细节属于服务端实现，让它可被回传等于让客户端指定 SQL 形状。
type BulkExecPlan struct {
	Set    map[string]any
	Filter []FilterCond
	// PK 是用于推进键集的主键列。只有单列主键才会启用分批。
	PK string
	// PKType 用于把读回来的主键值还原成列类型对应的 Go 值。
	PKType    model.ColumnType
	BatchSize int
}

// DefaultBulkBatchSize 是单批行数，与 bulkUndoLimit 同量级（docs/06 §3.2）。
const DefaultBulkBatchSize = 1000

// BulkProgress 是分批执行的进度回调。done 是已提交行数，total 是预案里的影响行数。
type BulkProgress func(done, total int64)

// applyBulkBatches 按主键键集分批执行批量修改。
//
// 三个刻意的选择：
//   - 每批一个独立事务，而不是一个大事务：两万行的长事务在 MySQL 上会长时间持锁，
//     且一旦中途失败要重来全部。代价是「不是原子」，所以必须报出已提交行数。
//   - 用主键键集推进（pk > last ORDER BY pk LIMIT n），而不是「改完再查一遍」：
//     若筛选条件与被改列无关，改过的行仍然命中，重查会原地打转。
//   - 每批仍然是 UPDATE ... WHERE pk IN (...)：键集只用来挑行，不用来改行。
//     直接 UPDATE ... WHERE pk > last 会跨过本批边界，把后面还没数过的行一起改掉。
//
// 中止：调用方取消 ctx 即停止，在批与批之间生效；已提交的批不回滚（docs/06 §3.2）。
// 因此中止后预案仍然有效，可以重新提交把剩下的一次性改完——
// SET 是常量，重复执行结果是幂等的。
func (e *Executor) applyBulkBatches(
	ctx context.Context,
	plan *ChangePlan,
	req Request,
	out *Outcome,
	started time.Time,
	progress BulkProgress,
) (*Outcome, error) {

	actor := actorOf(req.Actor)
	if plan.Bulk == nil {
		return nil, errors.New("不是分批批量修改预案")
	}

	td, err := e.conn.Meta().DescribeTable(ctx, plan.Table)
	if err != nil {
		return e.failBulk(out, actor, req, started, err)
	}
	known := columnCatalog(td)
	table := e.dialect.QuoteQualified(schemaOf(plan.Table, e.cfgSchema()), plan.Table.Name)

	// SET 子句
	setNames := sortedSetKeys(plan.Bulk.Set)
	assigns := make([]string, 0, len(setNames))
	setArgs := make([]any, 0, len(setNames))
	for _, name := range setNames {
		ci, ok := known[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			return e.failBulk(out, actor, req, started, fmt.Errorf("列 %q 不存在", name))
		}
		assigns = append(assigns, e.dialect.QuoteIdent(ci.name)+" = ?")
		v := plan.Bulk.Set[name]
		if s, ok := v.(string); ok {
			setArgs = append(setArgs, coerceKey(s, ci.typ))
		} else {
			setArgs = append(setArgs, v)
		}
	}
	setSQL := strings.Join(assigns, ", ")

	where, whereArgs, err := e.filterClause(known, plan.Bulk.Filter)
	if err != nil {
		return e.failBulk(out, actor, req, started, err)
	}

	pkIdent := e.dialect.QuoteIdent(plan.Bulk.PK)
	batch := plan.Bulk.BatchSize
	if batch <= 0 {
		batch = DefaultBulkBatchSize
	}

	var done int64
	var last any
	aborted := false

	for {
		if ctx.Err() != nil {
			aborted = true
			break
		}

		// 取下一批主键
		pageWhere := append([]string{}, where...)
		pageArgs := append([]any{}, whereArgs...)
		if last != nil {
			pageWhere = append(pageWhere, pkIdent+" > ?")
			pageArgs = append(pageArgs, last)
		}
		q := "SELECT " + pkIdent + " FROM " + table
		if len(pageWhere) > 0 {
			q += " WHERE " + strings.Join(pageWhere, " AND ")
		}
		q += " ORDER BY " + pkIdent + " LIMIT " + fmt.Sprint(batch)

		keys, err := e.readKeys(ctx, q, pageArgs, batch, plan.Bulk.PKType)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				aborted = true
				break
			}
			return e.failBulk(out, actor, req, started, err)
		}
		if len(keys) == 0 {
			break
		}

		marks := make([]string, len(keys))
		args := append([]any{}, setArgs...)
		for i, k := range keys {
			marks[i] = "?"
			args = append(args, k)
		}
		stmt := "UPDATE " + table + " SET " + setSQL + " WHERE " + pkIdent + " IN (" + strings.Join(marks, ", ") + ")"

		if err := e.execBatch(ctx, stmt, args, len(keys)); err != nil {
			if errors.Is(err, context.Canceled) {
				aborted = true
				break
			}
			out.Err = ClassifyError(err)
			out.Duration = time.Since(started)
			// 之前已经提交的批撤不回来，错误信息里必须带上这个数字。
			out.RowsAffected = done
			e.record(actor, req, out, "error", stmt,
				fmt.Sprintf("%s（此前已提交 %d 行，未回滚）", err.Error(), done))
			return out, nil
		}

		done += int64(len(keys))
		last = keys[len(keys)-1]
		if progress != nil {
			progress(done, plan.AffectedRows)
		}
		if len(keys) < batch {
			break
		}
	}

	out.Duration = time.Since(started)
	out.RowsAffected = done
	if aborted {
		// 中止不是失败：已经提交的批留在库里，必须让人看见「改了哪些、还剩多少」。
		out.Aborted = true
		out.Reason = fmt.Sprintf("已中止：已提交 %d 行（未回滚），其余行未改动", done)
		e.record(actor, req, out, "aborted", plan.Table.Name, out.Reason)
		return out, nil
	}
	e.record(actor, req, out, "ok", plan.Table.Name, fmt.Sprintf("分批修改 %d 行", done))
	return out, nil
}

// readKeys 读一批主键值。
//
// 驱动对 TEXT / UUID 可能返回 []byte，直接拿去当参数在 PG 上会被当成 bytea，
// 所以按已知列类型还原一次。
func (e *Executor) readKeys(ctx context.Context, q string, args []any, limit int, ct model.ColumnType) ([]any, error) {
	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()

	cur, err := sess.Query(ctx, q, args, driver.QueryOptions{FetchSize: limit})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close() }()

	rows, _, err := cur.Next(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]any, 0, len(rows))
	for _, r := range rows {
		if len(r) == 0 {
			continue
		}
		keys = append(keys, keyValue(r[0], ct))
	}
	return keys, nil
}

// keyValue 把读回来的主键值还原成可安全回传的参数。
func keyValue(v any, ct model.ColumnType) any {
	b, ok := v.([]byte)
	if !ok {
		return v
	}
	switch ct.Kind {
	case model.TypeString, model.TypeText, model.TypeEnum, model.TypeUUID:
		return string(b)
	}
	return v
}

// execBatch 在一个独立事务里执行一批更新，行数必须与本批主键数一致。
func (e *Executor) execBatch(ctx context.Context, stmt string, args []any, expect int) error {
	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	if err := sess.Begin(ctx); err != nil {
		return err
	}
	res, err := sess.Execute(ctx, stmt, args)
	if err != nil {
		_ = sess.Rollback(ctx)
		return err
	}
	if res.RowsAffected != int64(expect) {
		_ = sess.Rollback(ctx)
		return &ExecError{
			Code: "CHANGE_ROW_CONFLICT", Category: "constraint",
			Message: ErrRowConflict.Error(),
			Hint:    fmt.Sprintf("本批影响 %d 行，期望 %d 行", res.RowsAffected, expect),
		}
	}
	return sess.Commit(ctx)
}

func (e *Executor) failBulk(out *Outcome, actor string, req Request, started time.Time, err error) (*Outcome, error) {
	out.Err = ClassifyError(err)
	out.Duration = time.Since(started)
	e.record(actor, req, out, "error", "bulk", err.Error())
	return out, nil
}
