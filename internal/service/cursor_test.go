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
