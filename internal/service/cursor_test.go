package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// seed 在 t 表里写入 n 行（id 从 start 开始）。
func seed(t *testing.T, e *env, start, n int) {
	t.Helper()
	ctx := context.Background()
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	for i := start; i < start+n; i++ {
		if _, err := s.Execute(ctx, `INSERT INTO t (id, v) VALUES (?, 'x')`, []any{i}); err != nil {
			t.Fatalf("插入 id=%d: %v", i, err)
		}
	}
}

func TestQueryCursorPagination(t *testing.T) {
	e := setup(t)
	seed(t, e, 2, 24) // 表里已有 id=1，共 25 行

	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	out, err := ex.Query(context.Background(), service.Request{
		SQL:       `SELECT id FROM t ORDER BY id`,
		FetchSize: 5,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if out.CursorID == "" {
		t.Fatal("未返回游标 id")
	}
	if len(out.Columns) != 1 {
		t.Fatalf("列数 = %d", len(out.Columns))
	}
	if out.Risk.String() != "R0" {
		t.Errorf("Risk = %s, 期望 R0", out.Risk)
	}

	seen := map[int64]bool{}
	pages := 0
	for {
		pg, err := ex.Page(context.Background(), out.CursorID, 10)
		if err != nil {
			t.Fatalf("Page: %v", err)
		}
		rows, hasMore := pg.Rows, pg.HasMore
		pages++
		for _, r := range rows {
			id := r[0].(int64)
			if seen[id] {
				t.Fatalf("分页出现重复行: %d", id)
			}
			seen[id] = true
		}
		if !hasMore {
			break
		}
		if pages > 10 {
			t.Fatal("分页未收敛")
		}
	}

	if len(seen) != 25 {
		t.Errorf("共取到 %d 行, 期望 25", len(seen))
	}
	if pages != 3 {
		t.Errorf("页数 = %d, 期望 3（10+10+5）", pages)
	}
}

func TestQueryRejectsWriteStatement(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	if _, err := ex.Query(context.Background(), service.Request{SQL: `UPDATE t SET v='x' WHERE id=1`}); err == nil {
		t.Fatal("流式查询接口应拒绝写语句")
	}
}

func TestQueryRejectsMultipleStatements(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	if _, err := ex.Query(context.Background(), service.Request{SQL: `SELECT 1; SELECT 2`}); err == nil {
		t.Fatal("流式查询接口应拒绝多语句")
	}
}

func TestPageAfterExhaustedIsClosed(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	out, err := ex.Query(context.Background(), service.Request{SQL: `SELECT id FROM t`, FetchSize: 100})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if pg, err := ex.Page(context.Background(), out.CursorID, 100); err != nil || pg.HasMore {
		t.Fatalf("第一页应读尽: hasMore=%v err=%v", pg != nil && pg.HasMore, err)
	}
	if _, err := ex.Page(context.Background(), out.CursorID, 100); !errors.Is(err, service.ErrCursorClosed) {
		t.Fatalf("读尽后应返回 ErrCursorClosed, 实际 %v", err)
	}
}

func TestCloseCursor(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	out, err := ex.Query(context.Background(), service.Request{SQL: `SELECT id FROM t`})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !ex.CloseCursor(out.CursorID) {
		t.Fatal("关闭游标应返回 true")
	}
	if ex.CloseCursor(out.CursorID) {
		t.Fatal("重复关闭应返回 false")
	}
}

func TestTableRowsKeyset(t *testing.T) {
	e := setup(t)
	seed(t, e, 2, 29) // 共 30 行

	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	var after []any
	seen := map[int64]bool{}
	pages := 0

	for {
		res, err := ex.TableRows(context.Background(), service.TableRowsRequest{
			Table: "t", Limit: 10, After: after,
		})
		if err != nil {
			t.Fatalf("TableRows: %v", err)
		}
		pages++
		for _, r := range res.Rows {
			id := r[0].(int64)
			if seen[id] {
				t.Fatalf("键集分页出现重复行: %d", id)
			}
			seen[id] = true
		}
		if !res.HasMore {
			break
		}
		if len(res.Next) != 1 {
			t.Fatalf("下一页游标长度 = %d, 期望 1", len(res.Next))
		}
		after = res.Next
		if pages > 10 {
			t.Fatal("分页未收敛")
		}
	}

	if len(seen) != 30 {
		t.Errorf("共取到 %d 行, 期望 30", len(seen))
	}
	if pages != 3 {
		t.Errorf("页数 = %d, 期望 3", pages)
	}
}

func TestTableRowsFirstPageOrder(t *testing.T) {
	e := setup(t)
	seed(t, e, 2, 9)

	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	res, err := ex.TableRows(context.Background(), service.TableRowsRequest{Table: "t", Limit: 3})
	if err != nil {
		t.Fatalf("TableRows: %v", err)
	}
	if len(res.Rows) != 3 {
		t.Fatalf("行数 = %d, 期望 3", len(res.Rows))
	}
	for i, r := range res.Rows {
		want := int64(i + 1)
		if got := r[0].(int64); got != want {
			t.Errorf("第 %d 行 id = %d, 期望 %d", i, got, want)
		}
	}
	if !res.HasMore {
		t.Error("还有更多行时 HasMore 应为 true")
	}
}

func TestTableRowsNoPrimaryKey(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `CREATE TABLE nopk (a TEXT, b TEXT)`, nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	_ = s.Close()

	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	_, err = ex.TableRows(ctx, service.TableRowsRequest{Table: "nopk", Limit: 10})
	if !errors.Is(err, service.ErrNoPrimaryKey) {
		t.Fatalf("无主键表应返回 ErrNoPrimaryKey, 实际 %v", err)
	}
}

// TestTableRowsEditableFollowsAccessMode 验证「可编辑」不只是有主键，
// 还要连接真的可写：只读连接上返回 Editable=true 会让用户在界面上改一片，
// 提交时才被拒，属于把人往坑里带。
func TestTableRowsEditableFollowsAccessMode(t *testing.T) {
	e := setup(t)
	seed(t, e, 2, 2)
	ctx := context.Background()

	ro := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadOnly, nil)
	defer ro.Close()
	res, err := ro.TableRows(ctx, service.TableRowsRequest{Table: "t", Limit: 10})
	if err != nil {
		t.Fatalf("TableRows(只读): %v", err)
	}
	if res.Editable {
		t.Error("只读连接上 Editable 应为 false")
	}
	if len(res.PrimaryKey) != 1 || res.PrimaryKey[0] != "id" {
		t.Errorf("PrimaryKey = %v, 期望 [id]", res.PrimaryKey)
	}

	rw := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer rw.Close()
	res, err = rw.TableRows(ctx, service.TableRowsRequest{Table: "t", Limit: 10})
	if err != nil {
		t.Fatalf("TableRows(可写): %v", err)
	}
	if !res.Editable {
		t.Error("可写连接上有主键时 Editable 应为 true")
	}
}

// TestTableRowsCoercesKey 验证游标值按列类型还原：
// 传字符串 "9" 时对 INTEGER 主键必须按数字比较，否则 SQLite 会因类型序导致漏行。
func TestTableRowsCoercesKey(t *testing.T) {
	e := setup(t)
	seed(t, e, 2, 9) // id 1..10

	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	res, err := ex.TableRows(context.Background(), service.TableRowsRequest{
		Table: "t", Limit: 100, After: []any{"9"},
	})
	if err != nil {
		t.Fatalf("TableRows: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("id>9 应只剩 1 行, 实际 %d 行", len(res.Rows))
	}
	if got := res.Rows[0][0].(int64); got != 10 {
		t.Errorf("id = %d, 期望 10", got)
	}
}

func TestTableRowsFilter(t *testing.T) {
	e := setup(t)
	seed(t, e, 2, 9) // id 1..10，v 全为 'x'
	ctx := context.Background()

	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	// 整型列的比较必须按数字走：传字符串 "3" 若被当成文本，>= 会命中错误集合。
	res, err := ex.TableRows(ctx, service.TableRowsRequest{
		Table: "t", Limit: 100,
		Filter: []service.FilterCond{{Column: "id", Op: service.FilterGe, Value: "8"}},
	})
	if err != nil {
		t.Fatalf("TableRows: %v", err)
	}
	if len(res.Rows) != 3 {
		t.Fatalf("id>=8 应有 3 行, 实际 %d", len(res.Rows))
	}
	if got := res.Rows[0][0].(int64); got != 8 {
		t.Errorf("首行 id = %d, 期望 8", got)
	}

	// LIKE 只对文本有意义，不能被类型还原吃掉。
	res, err = ex.TableRows(ctx, service.TableRowsRequest{
		Table: "t", Limit: 100,
		Filter: []service.FilterCond{{Column: "v", Op: service.FilterLike, Value: "x%"}},
	})
	if err != nil {
		t.Fatalf("TableRows(like): %v", err)
	}
	if len(res.Rows) != 9 {
		t.Errorf("v LIKE 'x%%' 应有 9 行（seed 写入的 2..10）, 实际 %d", len(res.Rows))
	}

	res, err = ex.TableRows(ctx, service.TableRowsRequest{
		Table: "t", Limit: 100,
		Filter: []service.FilterCond{{Column: "v", Op: service.FilterLike, Value: "y%"}},
	})
	if err != nil {
		t.Fatalf("TableRows(like 无命中): %v", err)
	}
	if len(res.Rows) != 0 {
		t.Errorf("v LIKE 'y%%' 应 0 行, 实际 %d", len(res.Rows))
	}
}

func TestTableRowsFilterRejectsUnknownColumnAndOp(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	_, err := ex.TableRows(context.Background(), service.TableRowsRequest{
		Table: "t", Limit: 10,
		Filter: []service.FilterCond{{Column: "nope", Op: service.FilterEq, Value: "1"}},
	})
	if !errors.Is(err, service.ErrInvalidFilter) {
		t.Fatalf("未知列应返回 ErrInvalidFilter, 实际 %v", err)
	}

	_, err = ex.TableRows(context.Background(), service.TableRowsRequest{
		Table: "t", Limit: 10,
		Filter: []service.FilterCond{{Column: "id", Op: service.FilterOp("regexp"), Value: "1"}},
	})
	if !errors.Is(err, service.ErrInvalidFilter) {
		t.Fatalf("未知操作符应返回 ErrInvalidFilter, 实际 %v", err)
	}

	_, err = ex.TableRows(context.Background(), service.TableRowsRequest{
		Table: "t", Limit: 10, Order: &service.OrderSpec{Column: "nope"},
	})
	if !errors.Is(err, service.ErrInvalidFilter) {
		t.Fatalf("未知排序列应返回 ErrInvalidFilter, 实际 %v", err)
	}
}

// TestTableRowsSortPaginatesWithoutLoss 是本组最关键的一条：
// 按「非唯一列」排序时，若排序键里不带主键，键集分页会漏行或原地打转。
func TestTableRowsSortPaginatesWithoutLoss(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// 12 行只有 3 个不同的 v 值，必然出现重复排序键。
	for i := 2; i <= 12; i++ {
		v := []string{"aa", "bb", "cc"}[(i-1)%3]
		if _, err := s.Execute(ctx, `INSERT INTO t (id, v) VALUES (?, ?)`, []any{i, v}); err != nil {
			t.Fatalf("插入 id=%d: %v", i, err)
		}
	}
	_ = s.Close()

	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	for _, desc := range []bool{false, true} {
		order := &service.OrderSpec{Column: "v", Desc: desc}
		seen := map[int64]int{}
		var after []any
		for page := 0; ; page++ {
			res, err := ex.TableRows(ctx, service.TableRowsRequest{
				Table: "t", Limit: 5, After: after, Order: order,
			})
			if err != nil {
				t.Fatalf("desc=%v 第 %d 页: %v", desc, page, err)
			}
			for _, r := range res.Rows {
				seen[r[0].(int64)]++
			}
			if !res.HasMore {
				break
			}
			after = res.Next
			if page > 10 {
				t.Fatalf("desc=%v 翻页未终止", desc)
			}
		}
		if len(seen) != 12 {
			t.Fatalf("desc=%v 只翻到 %d 行, 期望 12", desc, len(seen))
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("desc=%v id=%d 出现 %d 次", desc, id, n)
			}
		}
	}
}

// TestTableRowsFilterInAndBetween 覆盖 docs/06 §4.1 里此前一直没做的两个操作符。
//
// IN 用 Values 而不是把逗号拼进 Value：「值里带逗号」是真实数据，
// 拼起来就没法还原成一个还是两个值。
func TestTableRowsFilterInAndBetween(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for i, v := range []string{"a", "b", "c", "a,b", "d"} {
		if _, err := s.Execute(ctx, `INSERT INTO t (id, v) VALUES (?, ?)`, []any{i + 2, v}); err != nil {
			t.Fatalf("造数: %v", err)
		}
	}
	_ = s.Close()

	ids := func(res *service.TableRowsResult) []int64 {
		out := make([]int64, 0, len(res.Rows))
		for _, r := range res.Rows {
			if n, ok := r[0].(int64); ok {
				out = append(out, n)
			}
		}
		return out
	}

	// IN：三个值，其中一个是「带逗号的单个值」，必须当成一个值而不是两个。
	res, err := ex.TableRows(ctx, service.TableRowsRequest{
		Table: "t",
		Filter: []service.FilterCond{{
			Column: "v", Op: service.FilterIn, Values: []string{"a", "c", "a,b"},
		}},
	})
	if err != nil {
		t.Fatalf("IN 查询: %v", err)
	}
	// setup() 已经插了一行 (1,'a')，所以命中是 1、2、4、5。
	got := ids(res)
	want := []int64{1, 2, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("IN 命中 %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("IN 命中 %v, 期望 %v", got, want)
		}
	}

	// BETWEEN：闭区间。
	res, err = ex.TableRows(ctx, service.TableRowsRequest{
		Table: "t",
		Filter: []service.FilterCond{{
			Column: "id", Op: service.FilterBetween, Values: []string{"2", "4"},
		}},
	})
	if err != nil {
		t.Fatalf("BETWEEN 查询: %v", err)
	}
	got = ids(res)
	if len(got) != 3 || got[0] != 2 || got[2] != 4 {
		t.Errorf("BETWEEN 命中 %v, 期望 id 2..4", got)
	}

	// 参数不合法要报 ErrInvalidFilter，而不是悄悄变成「不过滤」。
	if _, err := ex.TableRows(ctx, service.TableRowsRequest{
		Table:  "t",
		Filter: []service.FilterCond{{Column: "v", Op: service.FilterIn}},
	}); !errors.Is(err, service.ErrInvalidFilter) {
		t.Errorf("空 IN 应报 ErrInvalidFilter, 实际 %v", err)
	}
	if _, err := ex.TableRows(ctx, service.TableRowsRequest{
		Table:  "t",
		Filter: []service.FilterCond{{Column: "id", Op: service.FilterBetween, Values: []string{"1"}}},
	}); !errors.Is(err, service.ErrInvalidFilter) {
		t.Errorf("BETWEEN 少一个值应报 ErrInvalidFilter, 实际 %v", err)
	}
}
