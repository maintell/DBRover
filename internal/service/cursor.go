package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
)

var (
	// ErrCursorClosed 表示游标已关闭或超过存活期。
	ErrCursorClosed = errors.New("游标已关闭或已过期")
	// ErrCursorBusy 表示游标正被另一个请求读取。
	ErrCursorBusy = errors.New("游标正在被读取")
	// ErrNoPrimaryKey 表示表没有主键，无法做键集分页。
	ErrNoPrimaryKey = errors.New("该表没有主键，无法做键集分页")
)

// defaultCursorTTL 是游标存活期。每次拉取会续期。
const defaultCursorTTL = 5 * time.Minute

// QueryOutcome 是一次流式查询的打开结果。
// 行数据不经由本结构返回，而是通过 CursorID 分页拉取。
type QueryOutcome struct {
	ExecutionID string
	Columns     []model.Column
	CursorID    string
	Risk        policy.Level
	Friction    policy.Friction
}

type cursorHandle struct {
	cur    *pagedCursor
	sess   driver.Session
	cancel context.CancelFunc
}

func (h *cursorHandle) close() {
	if h.cancel != nil {
		h.cancel()
	}
	if h.cur != nil {
		_ = h.cur.close()
	}
	if h.sess != nil {
		_ = h.sess.Close()
	}
}

// pagedCursor 在驱动游标之上提供「按需取 n 行」的能力，
// 因为驱动侧的批大小在创建时固定，而调用方每次可能请求不同页大小。
// Page 是一页结果。
type Page struct {
	Rows    [][]any
	HasMore bool
	// Truncated 表示结果因行数上限而停止，不是读尽。
	Truncated bool
}

type pagedCursor struct {
	c         driver.Cursor
	buf       [][]any
	done      bool
	truncated bool
}

func (p *pagedCursor) take(ctx context.Context, limit int) ([][]any, bool, error) {
	if limit <= 0 {
		limit = 200
	}
	for len(p.buf) < limit && !p.done {
		rows, more, err := p.c.Next(ctx)
		if err != nil {
			return nil, false, err
		}
		p.buf = append(p.buf, rows...)
		if !more {
			p.done = true
		}
	}
	if tr, ok := p.c.(driver.TruncationReporter); ok && tr.Truncated() {
		p.truncated = true
	}
	n := limit
	if n > len(p.buf) {
		n = len(p.buf)
	}
	out := p.buf[:n]
	p.buf = p.buf[n:]
	hasMore := len(p.buf) > 0 || !p.done
	return out, hasMore, nil
}

func (p *pagedCursor) close() error { return p.c.Close() }

type cursorItem struct {
	h       *cursorHandle
	touched time.Time
	busy    bool
}

type cursorStore struct {
	mu    sync.Mutex
	items map[string]*cursorItem
	ttl   time.Duration
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
}

func newCursorStore(ttl time.Duration) *cursorStore {
	if ttl <= 0 {
		ttl = defaultCursorTTL
	}
	s := &cursorStore{
		items: map[string]*cursorItem{},
		ttl:   ttl,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	// 必须有独立清理：只在 page 里顺带 sweep 的话，
	// 「打开后不再翻页」的游标永远不会过期，会一直占着会话与连接。
	go s.sweepLoop()
	return s
}

func (s *cursorStore) sweepLoop() {
	interval := s.ttl / 5
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	defer close(s.done)

	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			s.sweepLocked()
			s.mu.Unlock()
		}
	}
}

func (s *cursorStore) open(h *cursorHandle) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 必须是全局唯一 ID：多个连接的执行器各有自己的 store，
	// 而 API 层用游标 ID 做全局路由，用自增序号会串库。
	id := "cur-" + newID()
	s.items[id] = &cursorItem{h: h, touched: time.Now()}
	return id
}

func (s *cursorStore) page(ctx context.Context, id string, limit int) (*Page, error) {
	s.mu.Lock()
	s.sweepLocked()
	it, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return nil, ErrCursorClosed
	}
	if it.busy {
		s.mu.Unlock()
		return nil, ErrCursorBusy
	}
	it.busy = true
	s.mu.Unlock()

	rows, hasMore, err := it.h.cur.take(ctx, limit)
	truncated := it.h.cur.truncated

	s.mu.Lock()
	it.busy = false
	if err == nil {
		it.touched = time.Now()
	}
	if err != nil || !hasMore {
		delete(s.items, id)
	}
	s.mu.Unlock()

	if err != nil {
		it.h.close()
		return nil, err
	}
	if !hasMore {
		it.h.close()
	}
	return &Page{Rows: rows, HasMore: hasMore, Truncated: truncated}, nil
}

func (s *cursorStore) close(id string) bool {
	s.mu.Lock()
	it, ok := s.items[id]
	delete(s.items, id)
	s.mu.Unlock()
	if ok {
		it.h.close()
	}
	return ok
}

func (s *cursorStore) closeAll() {
	s.mu.Lock()
	items := s.items
	s.items = map[string]*cursorItem{}
	s.mu.Unlock()
	for _, it := range items {
		it.h.close()
	}
	// 停掉清理协程；重复调用是安全的。
	s.once.Do(func() {
		close(s.stop)
		<-s.done
	})
}

func (s *cursorStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

func (s *cursorStore) sweepLocked() {
	now := time.Now()
	for id, it := range s.items {
		if it.busy {
			continue
		}
		if now.Sub(it.touched) > s.ttl {
			delete(s.items, id)
			it.h.close()
		}
	}
}

// Query 打开一个流式查询。只接受单条只读语句，行数据通过 Page 分页拉取。
func (e *Executor) Query(ctx context.Context, req Request) (*QueryOutcome, error) {
	started := time.Now()

	stmts, err := e.dialect.Split(req.SQL)
	if err != nil {
		return nil, fmt.Errorf("脚本切分失败: %w", err)
	}
	if len(stmts) != 1 {
		return nil, fmt.Errorf("流式查询一次只接受一条语句，实际收到 %d 条", len(stmts))
	}

	preps, maxLevel, anyWrite := e.gradeAll(ctx, stmts)
	if anyWrite {
		return nil, fmt.Errorf("该语句会修改数据（%s），请改用执行接口", preps[0].gr.Level)
	}

	friction := e.frictionFor(maxLevel, anyWrite, req, 1)

	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	// 游标的存活期长于创建它的请求，因此必须与请求 context 解绑，
	// 否则 HTTP 处理器一返回，游标就会被连带取消。
	qctx, cancel := context.WithCancel(context.Background())
	cur, err := sess.Query(qctx, stmts[0].Text, req.Args, driver.QueryOptions{
		FetchSize: req.FetchSize,
		MaxRows:   req.MaxRows,
	})
	if err != nil {
		cancel()
		_ = sess.Close()
		return nil, err
	}

	out := &QueryOutcome{
		ExecutionID: newID(),
		Columns:     cur.Columns(),
		CursorID: e.cursors.open(&cursorHandle{
			cur:    &pagedCursor{c: cur},
			sess:   sess,
			cancel: cancel,
		}),
		Risk:     maxLevel,
		Friction: friction,
	}
	e.record(actorOf(req.Actor), req, &Outcome{
		ExecutionID: out.ExecutionID, Risk: maxLevel, Friction: friction,
		Duration: time.Since(started),
	}, "ok", req.SQL, "streaming query opened")
	return out, nil
}

// Page 拉取一页。返回 HasMore=false 时游标已被关闭。
func (e *Executor) Page(ctx context.Context, cursorID string, limit int) (*Page, error) {
	return e.cursors.page(ctx, cursorID, limit)
}

// CloseCursor 主动关闭游标。不存在时返回 false。
func (e *Executor) CloseCursor(cursorID string) bool { return e.cursors.close(cursorID) }

// Close 释放执行器持有的全部资源（游标与会话）。
func (e *Executor) Close() { e.cursors.closeAll() }

// TableRowsRequest 是表数据浏览请求。
type TableRowsRequest struct {
	Table string
	Limit int
	// After 是上一页最后一行的主键值，空表示第一页。
	After []any
}

// TableRowsResult 是表数据浏览结果。
type TableRowsResult struct {
	Columns []model.Column
	Rows    [][]any
	// Next 是下一页的起点（本页最后一行的主键值）。HasMore 为 false 时无意义。
	Next    []any
	HasMore bool
	SQL     string
}

// TableRows 用键集分页浏览表数据。
//
// 之所以能这么做：查询由本工具生成（单表、显式列清单、按主键排序），
// 而不是对用户任意 SQL 做包裹 —— 后者在聚合/窗口函数/UNION 上必然出错。
func (e *Executor) TableRows(ctx context.Context, req TableRowsRequest) (*TableRowsResult, error) {
	td, err := e.conn.Meta().DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: req.Table})
	if err != nil {
		return nil, err
	}
	pk := td.PrimaryKeyColumns()
	if len(pk) == 0 {
		return nil, ErrNoPrimaryKey
	}
	pkType := make(map[string]model.ColumnType, len(pk))
	for _, c := range td.Columns {
		if c.IsPrimary {
			pkType[strings.ToLower(c.Name)] = c.Type
		}
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	cols := make([]string, len(td.Columns))
	for i, c := range td.Columns {
		cols[i] = e.dialect.QuoteIdent(c.Name)
	}
	order := make([]string, len(pk))
	for i, c := range pk {
		order[i] = e.dialect.QuoteIdent(c)
	}

	q := "SELECT " + strings.Join(cols, ", ") + " FROM " + quotePath(e, req.Table)
	args := []any{}
	if len(req.After) == len(pk) {
		if len(pk) == 1 {
			q += " WHERE " + order[0] + " > ?"
		} else {
			marks := make([]string, len(pk))
			for i := range pk {
				marks[i] = "?"
			}
			q += " WHERE (" + strings.Join(order, ", ") + ") > (" + strings.Join(marks, ", ") + ")"
		}
		for i, v := range req.After {
			args = append(args, coerceKey(v, pkType[strings.ToLower(pk[i])]))
		}
	}
	q += " ORDER BY " + strings.Join(order, ", ") + " LIMIT " + strconv.Itoa(limit+1)

	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()

	cur, err := sess.Query(ctx, q, args, driver.QueryOptions{FetchSize: limit + 1})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close() }()

	var rows [][]any
	for len(rows) <= limit {
		batch, more, err := cur.Next(ctx)
		if err != nil {
			return nil, err
		}
		rows = append(rows, batch...)
		if !more {
			break
		}
	}

	out := &TableRowsResult{Columns: cur.Columns(), SQL: q}
	if len(rows) > limit {
		out.HasMore = true
		rows = rows[:limit]
	}
	out.Rows = rows
	if out.HasMore && len(rows) > 0 {
		idx := map[string]int{}
		for i, c := range td.Columns {
			idx[strings.ToLower(c.Name)] = i
		}
		last := rows[len(rows)-1]
		next := make([]any, len(pk))
		for i, c := range pk {
			j, ok := idx[strings.ToLower(c)]
			if !ok || j >= len(last) {
				return nil, fmt.Errorf("主键列 %s 不在结果集中", c)
			}
			next[i] = last[j]
		}
		out.Next = next
	}
	return out, nil
}

// quotePath 引用可能带限定前缀的表名。
func quotePath(e *Executor, name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = e.dialect.QuoteIdent(p)
	}
	return strings.Join(parts, ".")
}

func actorOf(actor string) string {
	if actor == "" {
		return "human:local"
	}
	return actor
}

// coerceKey 把游标里的字符串主键还原成列类型对应的值。
// 字符串在传输上是无损的；直接传 float64 会在大整数主键上丢精度。
func coerceKey(v any, ct model.ColumnType) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	switch ct.Kind {
	case model.TypeInteger:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	case model.TypeFloat, model.TypeDecimal:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return s
}
