package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

// querier 同时被 *sql.DB 与 *sql.Tx 满足。
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type session struct {
	c *Connector

	mu     sync.Mutex
	tx     *sql.Tx
	cancel context.CancelFunc
	closed bool
}

func (s *session) q() (querier, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("sqlite: 会话已关闭")
	}
	if s.tx != nil {
		return s.tx, nil
	}
	s.c.mu.RLock()
	db := s.c.db
	s.c.mu.RUnlock()
	if db == nil {
		return nil, fmt.Errorf("sqlite: 连接未打开")
	}
	return db, nil
}

func (s *session) setCancel(fn context.CancelFunc) {
	s.mu.Lock()
	s.cancel = fn
	s.mu.Unlock()
}

// Query 执行查询并返回游标。取消 context 会中断引擎侧语句。
func (s *session) Query(ctx context.Context, sqlText string, args []any, opt driver.QueryOptions) (driver.Cursor, error) {
	q, err := s.q()
	if err != nil {
		return nil, err
	}

	qctx, cancel := context.WithCancel(ctx)
	s.setCancel(cancel)

	rows, err := q.QueryContext(qctx, sqlText, args...)
	if err != nil {
		cancel()
		s.setCancel(nil)
		return nil, err
	}

	cols, err := describeCursor(rows)
	if err != nil {
		_ = rows.Close()
		cancel()
		s.setCancel(nil)
		return nil, err
	}

	fetch := opt.FetchSize
	if fetch <= 0 {
		fetch = 200
	}

	return &cursor{
		rows:    rows,
		cols:    cols,
		fetch:   fetch,
		maxRows: opt.MaxRows,
		cancel:  cancel,
		onClose: func() { s.setCancel(nil) },
	}, nil
}

func (s *session) Execute(ctx context.Context, sqlText string, args []any) (*driver.ExecResult, error) {
	q, err := s.q()
	if err != nil {
		return nil, err
	}
	ectx, cancel := context.WithCancel(ctx)
	s.setCancel(cancel)
	defer func() {
		cancel()
		s.setCancel(nil)
	}()

	res, err := q.ExecContext(ectx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	out := &driver.ExecResult{}
	if n, err := res.RowsAffected(); err == nil {
		out.RowsAffected = n
	}
	if id, err := res.LastInsertId(); err == nil {
		out.LastInsertID = id
		out.HasLastID = true
	}
	return out, nil
}

func (s *session) Begin(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("sqlite: 会话已关闭")
	}
	if s.tx != nil {
		return fmt.Errorf("sqlite: 已在事务中")
	}
	s.c.mu.RLock()
	db := s.c.db
	s.c.mu.RUnlock()
	if db == nil {
		return fmt.Errorf("sqlite: 连接未打开")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	s.tx = tx
	return nil
}

func (s *session) Commit(ctx context.Context) error {
	s.mu.Lock()
	tx := s.tx
	s.tx = nil
	s.mu.Unlock()
	if tx == nil {
		return fmt.Errorf("sqlite: 当前不在事务中")
	}
	return tx.Commit()
}

func (s *session) Rollback(ctx context.Context) error {
	s.mu.Lock()
	tx := s.tx
	s.tx = nil
	s.mu.Unlock()
	if tx == nil {
		return fmt.Errorf("sqlite: 当前不在事务中")
	}
	return tx.Rollback()
}

func (s *session) InTx() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tx != nil
}

// Cancel 中断当前在途语句。没有在途语句时是无操作。
func (s *session) Cancel(ctx context.Context) error {
	s.mu.Lock()
	c := s.cancel
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	c()
	return nil
}

// Close 关闭会话。未提交事务会被回滚。
func (s *session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	tx := s.tx
	s.tx = nil
	c := s.cancel
	s.cancel = nil
	s.mu.Unlock()

	if c != nil {
		c()
	}
	if tx != nil {
		return tx.Rollback()
	}
	return nil
}

type cursor struct {
	rows      *sql.Rows
	cols      []model.Column
	fetch     int
	maxRows   int
	sent      int
	done      bool
	truncated bool // 因 maxRows 停止，而不是读尽
	closed    bool
	cancel    context.CancelFunc
	onClose   func()
}

func (cur *cursor) Columns() []model.Column { return cur.cols }

// Truncated 报告是否因行数上限而提前结束（driver.TruncationReporter）。
func (cur *cursor) Truncated() bool { return cur.truncated }

func (cur *cursor) Next(ctx context.Context) ([][]any, bool, error) {
	if cur.closed || cur.done {
		return nil, false, nil
	}
	batch := make([][]any, 0, cur.fetch)
	for len(batch) < cur.fetch {
		if !cur.rows.Next() {
			cur.done = true
			break
		}
		vals := make([]any, len(cur.cols))
		ptrs := make([]any, len(cur.cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := cur.rows.Scan(ptrs...); err != nil {
			return nil, false, err
		}
		// 超过 2^53 的整数必须字符串化，否则 JSON 编码出去就已经丢了低位
		// （docs/05 §3.1）。
		driver.StringifyLargeInts(vals)
		batch = append(batch, vals)
		cur.sent++
		if cur.maxRows > 0 && cur.sent >= cur.maxRows {
			cur.done = true
			// 上限不是「读完」：必须标记，否则调用方当成全量。
			cur.truncated = true
			break
		}
	}
	if err := cur.rows.Err(); err != nil {
		return nil, false, err
	}
	return batch, !cur.done, nil
}

func (cur *cursor) Close() error {
	if cur.closed {
		return nil
	}
	cur.closed = true
	err := cur.rows.Close()
	if cur.cancel != nil {
		cur.cancel()
	}
	if cur.onClose != nil {
		cur.onClose()
	}
	return err
}

func describeCursor(rows *sql.Rows) ([]model.Column, error) {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	cols := make([]model.Column, len(cts))
	for i, ct := range cts {
		nullable, ok := ct.Nullable()
		if !ok {
			nullable = true
		}
		cols[i] = model.Column{
			Name:     ct.Name(),
			Type:     NeutralType(ct.DatabaseTypeName()),
			Nullable: nullable,
			Ordinal:  i,
		}
	}
	return cols, nil
}
