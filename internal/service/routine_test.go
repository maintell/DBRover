package service_test

import (
	"context"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestCreateProcedureOnRealMySQL 验证「不带 DELIMITER 的存储过程」能整条发过去。
//
// DELIMITER 是 mysql 客户端的语法，驱动不认；我们必须在切分器里认出 BEGIN…END，
// 否则这条语句会被体内的分号切成三截，第一条就报语法错。真机验一遍：过程建得成、
// CALL 之后效果对。
func TestCreateProcedureOnRealMySQL(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			// 按子用例跳：写在循环外层会把整条用例跳过，连 mysql 那条也不跑了。
			if tgt.name != "mysql" {
				t.Skip("只在 MySQL 上验：BEGIN…END 的体内分号语义是它的问题")
			}
			ctx := context.Background()
			tgt.prepareTable(t, []string{"DROP PROCEDURE IF EXISTS rt_p", "DROP TABLE IF EXISTS rt_t"}, nil)
			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()
			if err := execStrict(ctx, ex, "CREATE TABLE rt_t (id INTEGER PRIMARY KEY)"); err != nil {
				t.Fatalf("建表: %v", err)
			}
			t.Cleanup(func() {
				_ = execStrict(context.Background(), ex, "DROP PROCEDURE IF EXISTS rt_p")
				_ = execStrict(context.Background(), ex, "DROP TABLE IF EXISTS rt_t")
			})

			proc := `CREATE PROCEDURE rt_p()` + "\nBEGIN\n" +
				"  INSERT INTO rt_t (id) VALUES (1);\n" +
				"  INSERT INTO rt_t (id) VALUES (2);\n" +
				"END"
			out, err := ex.Execute(ctx, service.Request{SQL: proc, Approval: true})
			if err != nil || out == nil || out.Err != nil {
				t.Fatalf("建存储过程: %v %+v", err, out)
			}
			if len(out.Results) != 1 {
				t.Fatalf("应被当成一条语句，实际切成 %d 条——体内分号被当成分隔符了", len(out.Results))
			}
			if err := execStrict(ctx, ex, "CALL rt_p()"); err != nil {
				t.Fatalf("CALL: %v", err)
			}
			if n := countRows(t, ctx, ex, "SELECT count(*) FROM rt_t"); n != 2 {
				t.Fatalf("过程执行后应有 2 行，实际 %d", n)
			}
		})
	}
}

// TestCreateFunctionOnRealPG 验证 dollar-quoted 函数体同样是一条语句。
//
// PG 的函数体在词法层是一个 Token（$$…$$），所以没有「体内分号」问题；这条
// 真机用例守的是另一件事：整条 CREATE FUNCTION 能被接受并真的可调用。
func TestCreateFunctionOnRealPG(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			if tgt.name != "pg" {
				t.Skip("只在 PG 上验 dollar-quoted 函数体")
			}
			ctx := context.Background()
			tgt.prepareTable(t, []string{"DROP FUNCTION IF EXISTS rt_f(int)"}, nil)
			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()
			t.Cleanup(func() {
				_ = execStrict(context.Background(), ex, "DROP FUNCTION IF EXISTS rt_f(int)")
			})

			fn := "CREATE FUNCTION rt_f(n int) RETURNS int AS $$\nBEGIN\n  RETURN n * 2;\nEND;\n$$ LANGUAGE plpgsql"
			out, err := ex.Execute(ctx, service.Request{SQL: fn, Approval: true})
			if err != nil || out == nil || out.Err != nil {
				t.Fatalf("建函数: %v %+v", err, out)
			}
			if len(out.Results) != 1 {
				t.Fatalf("应被当成一条语句，实际 %d 条", len(out.Results))
			}
			if n := countRows(t, ctx, ex, "SELECT rt_f(21)"); n != 42 {
				t.Fatalf("函数结果 = %d，期望 42", n)
			}
		})
	}
}
