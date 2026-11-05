package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestContinueOnErrorKeepsGoing 锁住 docs/05 §2.1 的两种脚本语义。
//
// 默认「遇错即停」：第一条失败，后面的不执行——多语句脚本里第一条失败通常说明
// 前提不成立，闷头往下跑更容易把库改坏。
// 勾了「继续执行」后每条独立：失败那条带自己的 err，其余照常执行并保留结果。
func TestContinueOnErrorKeepsGoing(t *testing.T) {
	ctx := context.Background()
	script := "SELECT 1 AS a;\nSELECT * FROM 不存在的表;\nSELECT 3 AS c"

	t.Run("默认遇错即停", func(t *testing.T) {
		e := setup(t)
		ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
		out, err := ex.Execute(ctx, service.Request{SQL: script})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if out.Err == nil {
			t.Fatal("第二条语句不存在表，应带执行期错误")
		}
		if len(out.Results) != 2 {
			t.Fatalf("结果数 = %d，期望 2（第一条成功 + 第二条失败）", len(out.Results))
		}
		if out.Results[0].Err != "" {
			t.Errorf("第一条不该失败：%s", out.Results[0].Err)
		}
		if out.Results[1].Err == "" {
			t.Error("第二条应带错误")
		}
	})

	t.Run("继续执行", func(t *testing.T) {
		e := setup(t)
		ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
		out, err := ex.Execute(ctx, service.Request{SQL: script, ContinueOnError: true})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if out.Err == nil {
			t.Fatal("仍应带执行期错误：继续执行不等于「没有失败」")
		}
		if len(out.Results) != 3 {
			t.Fatalf("结果数 = %d，期望 3（三条都跑）", len(out.Results))
		}
		if out.Results[2].Err != "" {
			t.Errorf("第三条应成功，实际 %s", out.Results[2].Err)
		}
		if len(out.Results[2].Rows) != 1 {
			t.Errorf("第三条应带回一行，实际 %d", len(out.Results[2].Rows))
		}
	})
}

// TestMaxRowsCeiling 钉住 docs/05 §8 的行数上限：给再大的数也按 10 万算。
//
// 断言放在「真的取了多少行」上而不是配置值上：上限只有走到驱动才生效，
// 只测常量等于测自己写的注释。
func TestMaxRowsCeiling(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	out, err := ex.Execute(ctx, service.Request{SQL: "SELECT 1 AS a", MaxRows: 1 << 30})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Err != nil {
		t.Fatalf("执行失败: %+v", out.Err)
	}
	if len(out.Results) != 1 || len(out.Results[0].Rows) != 1 {
		t.Fatalf("结果形状不对：%+v", out.Results)
	}
}

// TestPageSizeCeiling 钉住单页上限：超过 2000 按 2000 处理。
func TestPageSizeCeiling(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	out, err := ex.Query(context.Background(), service.Request{SQL: "SELECT id, v FROM t"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	page, err := ex.Page(context.Background(), out.CursorID, 5000)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("应取回 1 行（表里只有一行），实际 %d", len(page.Rows))
	}
	ex.CloseCursor(out.CursorID)
}

// TestTableRowsRejectsNoLimitCrash 是「默认页大小」的护栏：不给 limit 时
// 不能变成「不限」，否则一次浏览就把整表读进内存。
func TestTableRowsDefaultLimit(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	res, err := ex.TableRows(context.Background(), service.TableRowsRequest{Table: "t"})
	if err != nil {
		t.Fatalf("TableRows: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("应取回 1 行，实际 %d", len(res.Rows))
	}
	if !strings.Contains(res.SQL, "LIMIT 201") {
		t.Errorf("默认页大小应是 200（SQL 里取 201 行判有无下一页），实际 SQL: %s", res.SQL)
	}
}
