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

// ErrInvalidFilter 表示筛选或排序参数不合法（列不存在 / 操作符不支持）。
// 单独成类型是为了让 API 层给出 400 而不是笼统的 500。
var ErrInvalidFilter = errors.New("筛选或排序参数不合法")

// FilterOp 是筛选操作符白名单。只接受这里列出的取值。
type FilterOp string

const (
	FilterEq      FilterOp = "eq"
	FilterNe      FilterOp = "ne"
	FilterGt      FilterOp = "gt"
	FilterGe      FilterOp = "ge"
	FilterLt      FilterOp = "lt"
	FilterLe      FilterOp = "le"
	FilterLike    FilterOp = "like"
	FilterIsNull  FilterOp = "is_null"
	FilterNotNull FilterOp = "is_not_null"
	// FilterIn 用 Values 而不是 Value：`IN ('a,b')` 会把「一个带逗号的字符串」
	// 和「两个值」混成一件事，这种歧义在数据工具里迟早出事。
	FilterIn FilterOp = "in"
	// FilterBetween 用 Values[0]..Values[1]，闭区间，与 SQL 的 BETWEEN 一致。
	FilterBetween FilterOp = "between"
)

// filterSQL 把操作符映射到 SQL 记号。写法固定，不接受调用方传入的符号，
// 避免把 WHERE 片段当成字符串拼进去。
var filterSQL = map[FilterOp]string{
	FilterEq:   "=",
	FilterNe:   "<>",
	FilterGt:   ">",
	FilterGe:   ">=",
	FilterLt:   "<",
	FilterLe:   "<=",
	FilterLike: "LIKE",
}

// FilterCond 是一个筛选条件。
//
// Value 用于单值操作符；Values 用于 in / between。分两个字段而不是一个字符串
// 拼逗号：「值里带逗号」是真实存在的数据，拼起来就没法还原。
type FilterCond struct {
	Column string   `json:"column"`
	Op     FilterOp `json:"op"`
	Value  string   `json:"value"`
	// Values 供 in（任意个）与 between（恰好两个）使用。
	Values []string `json:"values,omitempty"`
}

// OrderSpec 是单列排序。未指定时按主键排序。
type OrderSpec struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc"`
}

// TableRowsRequest 是表数据浏览请求。
type TableRowsRequest struct {
	Table string
	Limit int
	// After 是上一页最后一行的定位值，顺序与「排序列…, 主键…」一致；空表示第一页。
	After []any
	// Filter 是 AND 组合的筛选条件。不支持 OR 与分组（见 docs/06 §4.3）。
	Filter []FilterCond
	// Order 非空时按该列排序；主键仍会追加在排序键末尾，否则键集分页会在
	// 重复值上漏行或原地打转。
	Order *OrderSpec
}

// TableRowsResult 是表数据浏览结果。
type TableRowsResult struct {
	Columns []model.Column
	Rows    [][]any
	// Next 是下一页的起点（本页最后一行的「排序列…, 主键…」取值）。HasMore 为 false 时无意义。
	Next    []any
	HasMore bool
	SQL     string
	// PrimaryKey 是主键列名。前端据此构造行定位条件。
	PrimaryKey []string
	// Editable 表示这张表能否安全地行级编辑（必须有主键且连接可写）。
	Editable bool
}

// columnRef 是一列在结果集中的位置与定义。
// 筛选与排序只允许命中表定义里存在的列，因此不需要把调用方字符串拼进 SQL。
type columnRef struct {
	name  string
	index int
	typ   model.ColumnType
}

// columnCatalog 按小写列名建索引，调用方传什么大小写都能对上。
func columnCatalog(td *model.TableDetail) map[string]columnRef {
	out := make(map[string]columnRef, len(td.Columns))
	for i, c := range td.Columns {
		out[strings.ToLower(c.Name)] = columnRef{name: c.Name, index: i, typ: c.Type}
	}
	return out
}

// filterClause 把筛选条件编译成 WHERE 片段与参数。
// 列名走 QuoteIdent、操作符走白名单，值走参数绑定。
func (e *Executor) filterClause(known map[string]columnRef, filter []FilterCond) ([]string, []any, error) {
	where := make([]string, 0, len(filter))
	args := make([]any, 0, len(filter))
	for _, f := range filter {
		ci, ok := known[strings.ToLower(strings.TrimSpace(f.Column))]
		if !ok {
			return nil, nil, fmt.Errorf("%w: 筛选列 %q 不存在", ErrInvalidFilter, f.Column)
		}
		ident := e.dialect.QuoteIdent(ci.name)
		switch f.Op {
		case FilterIsNull:
			where = append(where, ident+" IS NULL")
		case FilterNotNull:
			where = append(where, ident+" IS NOT NULL")
		case FilterIn:
			if len(f.Values) == 0 {
				return nil, nil, fmt.Errorf("%w: IN 至少要给一个值", ErrInvalidFilter)
			}
			marks := make([]string, len(f.Values))
			for i, v := range f.Values {
				marks[i] = "?"
				args = append(args, coerceKey(v, ci.typ))
			}
			where = append(where, ident+" IN ("+strings.Join(marks, ", ")+")")
		case FilterBetween:
			if len(f.Values) != 2 {
				return nil, nil, fmt.Errorf("%w: BETWEEN 需要恰好两个值（闭区间）", ErrInvalidFilter)
			}
			where = append(where, ident+" BETWEEN ? AND ?")
			args = append(args, coerceKey(f.Values[0], ci.typ), coerceKey(f.Values[1], ci.typ))
		default:
			sym, ok := filterSQL[f.Op]
			if !ok {
				return nil, nil, fmt.Errorf("%w: 不支持的操作符 %q", ErrInvalidFilter, f.Op)
			}
			where = append(where, ident+" "+sym+" ?")
			if f.Op == FilterLike {
				// LIKE 只对文本有意义，不做类型还原，否则 "a%" 会被数字列解析掉。
				args = append(args, f.Value)
			} else {
				args = append(args, coerceKey(f.Value, ci.typ))
			}
		}
	}
	return where, args, nil
}

// orderClause 生成 ORDER BY 片段。列不存在时返回 ErrInvalidFilter。
func (e *Executor) orderClause(known map[string]columnRef, order *OrderSpec) ([]string, error) {
	if order == nil || strings.TrimSpace(order.Column) == "" {
		return nil, nil
	}
	ci, ok := known[strings.ToLower(strings.TrimSpace(order.Column))]
	if !ok {
		return nil, fmt.Errorf("%w: 排序列 %q 不存在", ErrInvalidFilter, order.Column)
	}
	ident := e.dialect.QuoteIdent(ci.name)
	if order.Desc {
		ident += " DESC"
	}
	return []string{ident}, nil
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
	// 列名 → 结果集下标 / 真实写法 / 类型。筛选与排序都只允许命中这里的列，
	// 因此不存在把调用方字符串直接拼进 SQL 的路径。
	known := columnCatalog(td)

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

	// 排序键 = 用户指定列（可选）+ 全部主键。主键必须在内，否则重复值上分页会漏行。
	desc := req.Order != nil && req.Order.Desc
	keyIdx := make([]columnRef, 0, len(pk)+1)
	if req.Order != nil && strings.TrimSpace(req.Order.Column) != "" {
		ci, ok := known[strings.ToLower(strings.TrimSpace(req.Order.Column))]
		if !ok {
			return nil, fmt.Errorf("%w: 排序列 %q 不存在", ErrInvalidFilter, req.Order.Column)
		}
		keyIdx = append(keyIdx, ci)
	}
	for _, c := range pk {
		if len(keyIdx) > 0 && strings.EqualFold(keyIdx[0].name, c) {
			continue // 排序列就是主键本身，不重复追加
		}
		ci, ok := known[strings.ToLower(c)]
		if !ok {
			return nil, fmt.Errorf("主键列 %s 不在表定义中", c)
		}
		keyIdx = append(keyIdx, ci)
	}

	orderParts := make([]string, len(keyIdx))
	keyCols := make([]string, len(keyIdx))
	for i, ci := range keyIdx {
		ident := e.dialect.QuoteIdent(ci.name)
		keyCols[i] = ident
		orderParts[i] = ident
		if desc {
			orderParts[i] += " DESC"
		}
	}

	where, args, err := e.filterClause(known, req.Filter)
	if err != nil {
		return nil, err
	}
	if len(req.After) == len(keyIdx) {
		cmp := ">"
		if desc {
			cmp = "<"
		}
		if len(keyIdx) == 1 {
			where = append(where, keyCols[0]+" "+cmp+" ?")
		} else {
			marks := make([]string, len(keyIdx))
			for i := range keyIdx {
				marks[i] = "?"
			}
			where = append(where, "("+strings.Join(keyCols, ", ")+") "+cmp+" ("+strings.Join(marks, ", ")+")")
		}
		for i, v := range req.After {
			args = append(args, coerceKey(v, keyIdx[i].typ))
		}
	}

	q := "SELECT " + strings.Join(cols, ", ") + " FROM " + quotePath(e, req.Table)
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY " + strings.Join(orderParts, ", ") + " LIMIT " + strconv.Itoa(limit+1)

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

	out := &TableRowsResult{
		Columns:    cur.Columns(),
		SQL:        q,
		PrimaryKey: pk,
		// 只读连接上不该出现可编辑单元格：否则用户改了一堆才发现提交被拒。
		Editable: e.mode.AllowsWrite(),
	}
	if len(rows) > limit {
		out.HasMore = true
		rows = rows[:limit]
	}
	out.Rows = rows
	if out.HasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		next := make([]any, len(keyIdx))
		for i, ci := range keyIdx {
			if ci.index >= len(last) {
				return nil, fmt.Errorf("定位列 %s 不在结果集中", ci.name)
			}
			next[i] = last[ci.index]
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
	case model.TypeDate, model.TypeTime, model.TypeTimestamp:
		return normalizeTemporal(s, ct)
	}
	return s
}

// normalizeTemporal 把时间值统一成「引擎能接受、且瞬间不会被悄悄挪动」的形态。
//
// 两组事实决定了这套规则：
//  1. 我们读出去的 timestamptz 是带偏移的 RFC3339（2026-11-03T12:00:00+08:00），
//     原样写回 MySQL 会报 1292 Incorrect datetime value——「读出来再存回去」
//     这条路必须通，否则行内编辑时间列根本用不了。
//  2. 带时区的列存的是**瞬间**，所以换算到 UTC 再写；不带时区的列存的是**日历
//     时间**（墙上时钟），换算反而会让用户看到的 12:00 变成 04:00，所以原样写。
//     DATE / TIME 同理取字面值，不跨零点。
func normalizeTemporal(s string, ct model.ColumnType) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// 已经是引擎原生写法（或用户手写），交给引擎判断，不猜。
		return s
	}
	switch ct.Kind {
	case model.TypeDate:
		return t.Format("2006-01-02")
	case model.TypeTime:
		return t.Format("15:04:05")
	case model.TypeTimestamp:
		if ct.WithTZ {
			// 会话时区已钉成 UTC，写 UTC 才是同一个瞬间。
			return t.UTC().Format("2006-01-02 15:04:05")
		}
		return t.Format("2006-01-02 15:04:05")
	}
	return s
}
