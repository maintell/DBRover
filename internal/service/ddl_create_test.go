package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

func TestDDLCreateTable(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	note := "'未命名'"
	plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("created"),
		Action: service.DDLCreateTable,
		Columns: []service.ColumnSpec{
			{Name: "id", Type: "INTEGER"},
			{Name: "label", Type: "TEXT", Default: &note},
			{Name: "score", Type: "INTEGER", Nullable: true},
		},
		PrimaryKey: []string{"id"},
	})
	if err != nil {
		t.Fatalf("BuildDDLPlan: %v", err)
	}
	if !plan.DDL {
		t.Error("应标记 ddl=true")
	}
	if plan.Irreversible {
		t.Error("建表不该标不可逆（回滚是删表，表还是空的）")
	}
	sqlText := plan.Statements[0].SQL
	if !strings.HasPrefix(sqlText, "CREATE TABLE ") {
		t.Errorf("语句不是建表: %s", sqlText)
	}
	if !strings.Contains(sqlText, `PRIMARY KEY ("id")`) {
		t.Errorf("缺主键约束: %s", sqlText)
	}
	// 主键列必须被强制 NOT NULL：否则「主键能不能为空」在引擎与工具之间会有两个答案。
	if !strings.Contains(sqlText, `"id" INTEGER NOT NULL`) {
		t.Errorf("主键列没有强制 NOT NULL: %s", sqlText)
	}
	if len(plan.Rollback) != 1 || !strings.HasPrefix(plan.Rollback[0].SQL, "DROP TABLE ") {
		t.Errorf("回滚语句不对: %+v", plan.Rollback)
	}

	out, err := ex.ApplyChangePlan(ctx, plan, service.Request{})
	if err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("应用失败: err=%v out=%+v", err, out)
	}

	detail, err := ex.DescribeTable(ctx, tableRef("created"))
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if len(detail.Columns) != 3 {
		t.Fatalf("列数 = %d, 期望 3", len(detail.Columns))
	}
	pk := detail.PrimaryKeyColumns()
	if len(pk) != 1 || pk[0] != "id" {
		t.Errorf("主键 = %v, 期望 [id]", pk)
	}

	// 表已存在时再建同名表必须被拒，而不是撞引擎报错。
	if _, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("created"),
		Action: service.DDLCreateTable,
		Columns: []service.ColumnSpec{
			{Name: "id", Type: "INTEGER"},
		},
		PrimaryKey: []string{"id"},
	}); !errors.Is(err, service.ErrInvalidDDL) {
		t.Errorf("同名建表应返回 ErrInvalidDDL, 实际 %v", err)
	}
}

func TestDDLCreateTableRejectsBadInput(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	cases := []struct {
		name string
		req  service.DDLRequest
	}{
		{"没有列", service.DDLRequest{
			Table: tableRef("x1"), Action: service.DDLCreateTable,
			PrimaryKey: []string{"id"},
		}},
		{"没有主键", service.DDLRequest{
			Table: tableRef("x2"), Action: service.DDLCreateTable,
			Columns: []service.ColumnSpec{{Name: "id", Type: "INTEGER"}},
		}},
		{"主键列不在列清单", service.DDLRequest{
			Table: tableRef("x3"), Action: service.DDLCreateTable,
			Columns:    []service.ColumnSpec{{Name: "id", Type: "INTEGER"}},
			PrimaryKey: []string{"nope"},
		}},
		{"列名重复", service.DDLRequest{
			Table: tableRef("x4"), Action: service.DDLCreateTable,
			Columns: []service.ColumnSpec{
				{Name: "id", Type: "INTEGER"},
				{Name: "ID", Type: "INTEGER"},
			},
			PrimaryKey: []string{"id"},
		}},
		{"主键重复", service.DDLRequest{
			Table: tableRef("x5"), Action: service.DDLCreateTable,
			Columns:    []service.ColumnSpec{{Name: "id", Type: "INTEGER"}},
			PrimaryKey: []string{"id", "id"},
		}},
		{"类型带注入", service.DDLRequest{
			Table: tableRef("x6"), Action: service.DDLCreateTable,
			Columns:    []service.ColumnSpec{{Name: "id", Type: "INTEGER; DROP TABLE t"}},
			PrimaryKey: []string{"id"},
		}},
		{"表名空", service.DDLRequest{
			Table: model.ObjectRef{Kind: model.KindTable}, Action: service.DDLCreateTable,
			Columns:    []service.ColumnSpec{{Name: "id", Type: "INTEGER"}},
			PrimaryKey: []string{"id"},
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ex.BuildDDLPlan(ctx, c.req)
			if !errors.Is(err, service.ErrInvalidDDL) {
				t.Fatalf("应返回 ErrInvalidDDL, 实际 %v", err)
			}
		})
	}
}

// TestDDLCreateTableBlocksWhenTableAppeared 验证建表的竞态保护：
// 预案生成后别人先建了同名表，执行时必须停下，而不是撞引擎的 already exists。
func TestDDLCreateTableBlocksWhenTableAppeared(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("racer"),
		Action: service.DDLCreateTable,
		Columns: []service.ColumnSpec{
			{Name: "id", Type: "INTEGER"},
		},
		PrimaryKey: []string{"id"},
	})
	if err != nil {
		t.Fatalf("BuildDDLPlan: %v", err)
	}

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `CREATE TABLE racer (id INTEGER PRIMARY KEY)`, nil); err != nil {
		t.Fatalf("模拟他人建表: %v", err)
	}
	_ = s.Close()

	out, err := ex.ApplyChangePlan(ctx, plan, service.Request{})
	if err != nil {
		t.Fatalf("ApplyChangePlan: %v", err)
	}
	if out.Err == nil || out.Err.Code != "SCHEMA_CHANGED" {
		t.Fatalf("应报 SCHEMA_CHANGED, 实际 out=%+v", out)
	}
}
