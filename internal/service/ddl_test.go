package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

func tableRef(name string) model.ObjectRef {
	return model.ObjectRef{Kind: model.KindTable, Name: name}
}

// columnNames 读回表的列名集合。
func columnNames(t *testing.T, e *env, table string) map[string]bool {
	t.Helper()
	detail, err := e.rw.Meta().DescribeTable(context.Background(), tableRef(table))
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	out := map[string]bool{}
	for _, c := range detail.Columns {
		out[strings.ToLower(c.Name)] = true
	}
	return out
}

func TestDDLPlanAddColumn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	nullable := true
	plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLAddColumn,
		Column: service.ColumnSpec{Name: "note", Type: "TEXT", Nullable: nullable},
	})
	if err != nil {
		t.Fatalf("BuildDDLPlan: %v", err)
	}
	if !plan.DDL {
		t.Error("结构变更预案应标记 ddl=true")
	}
	if len(plan.Statements) != 1 {
		t.Fatalf("语句数 = %d", len(plan.Statements))
	}
	sqlText := plan.Statements[0].SQL
	if !strings.Contains(sqlText, `ALTER TABLE`) || !strings.Contains(sqlText, `"note"`) {
		t.Errorf("语句不像加列: %s", sqlText)
	}
	if plan.Risk.String() != "R3" {
		t.Errorf("加列风险 = %s, 期望 R3", plan.Risk)
	}
	if plan.Irreversible {
		t.Error("加列应当可回滚（回滚是删列）")
	}
	if len(plan.Rollback) != 1 || !strings.Contains(plan.Rollback[0].SQL, `DROP COLUMN`) {
		t.Errorf("回滚语句不对: %+v", plan.Rollback)
	}

	out, err := ex.ApplyChangePlan(ctx, plan, service.Request{})
	if err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("应用失败: err=%v out=%+v", err, out)
	}
	if !columnNames(t, e, "t")["note"] {
		t.Fatal("加列后看不到 note 列")
	}
}

func TestDDLPlanDropColumnIsIrreversible(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	// 造两行，其中一行 v 为空：非空行数应当是 1 而不是 2 或 0。
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO t (id, v) VALUES (2, NULL)`,
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("准备数据: %v", err)
		}
	}
	_ = s.Close()

	ex := newTestExecutor(t, e)
	plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLDropColumn,
		Column: service.ColumnSpec{Name: "v"},
	})
	if err != nil {
		t.Fatalf("BuildDDLPlan: %v", err)
	}
	if !plan.Irreversible {
		t.Error("删列必须标记不可逆：数据回不来")
	}
	if plan.AffectedRows != 1 {
		t.Errorf("非空行数 = %d, 期望 1", plan.AffectedRows)
	}

	out, err := ex.ApplyChangePlan(ctx, plan, service.Request{})
	if err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("应用失败: err=%v out=%+v", err, out)
	}
	if columnNames(t, e, "t")["v"] {
		t.Fatal("删列后 v 仍在")
	}
}

func TestDDLPlanRejectsBadInput(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	cases := []struct {
		name string
		req  service.DDLRequest
	}{
		{"类型里塞分号", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLAddColumn,
			Column: service.ColumnSpec{Name: "x", Type: "TEXT; DROP TABLE t"},
		}},
		{"列名带引号", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLAddColumn,
			Column: service.ColumnSpec{Name: `a"b`, Type: "TEXT"},
		}},
		{"列名带反斜杠", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLAddColumn,
			Column: service.ColumnSpec{Name: `a\b`, Type: "TEXT"},
		}},
		{"默认值是表达式", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLAddColumn,
			Column: func() service.ColumnSpec {
				d := "nextval('s')"
				return service.ColumnSpec{Name: "x", Type: "INTEGER", Default: &d}
			}(),
		}},
		{"加已存在的列", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLAddColumn,
			Column: service.ColumnSpec{Name: "v", Type: "TEXT"},
		}},
		{"删不存在的列", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLDropColumn,
			Column: service.ColumnSpec{Name: "nope"},
		}},
		{"删主键列", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLDropColumn,
			Column: service.ColumnSpec{Name: "id"},
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

// TestDDLPlanSchemaChangedBlocks 验证前置条件：
// 预案生成后表定义被别人改了，执行必须被拒，而不是照着旧结构硬跑。
func TestDDLPlanSchemaChangedBlocks(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLAddColumn,
		Column: service.ColumnSpec{Name: "note", Type: "TEXT", Nullable: true},
	})
	if err != nil {
		t.Fatalf("BuildDDLPlan: %v", err)
	}

	// 模拟另一个会话改了表结构。
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `ALTER TABLE t ADD COLUMN other TEXT`, nil); err != nil {
		t.Fatalf("改结构: %v", err)
	}
	_ = s.Close()

	out, err := ex.ApplyChangePlan(ctx, plan, service.Request{})
	if err != nil {
		t.Fatalf("ApplyChangePlan: %v", err)
	}
	if out.Err == nil || out.Err.Code != "SCHEMA_CHANGED" {
		t.Fatalf("应报 SCHEMA_CHANGED, 实际 out=%+v", out)
	}
	if columnNames(t, e, "t")["note"] {
		t.Fatal("被拦下时不应真的加列")
	}
}

// TestDDLPlanAddColumnWithDefault 验证默认值白名单同时能放过合法字面量。
func TestDDLPlanAddColumnWithDefault(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	def := "'未命名'"
	plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLAddColumn,
		Column: service.ColumnSpec{Name: "label", Type: "TEXT", Default: &def},
	})
	if err != nil {
		t.Fatalf("BuildDDLPlan: %v", err)
	}
	if !strings.Contains(plan.Statements[0].SQL, "DEFAULT '未命名'") {
		t.Errorf("默认值没进语句: %s", plan.Statements[0].SQL)
	}

	out, err := ex.ApplyChangePlan(ctx, plan, service.Request{})
	if err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("应用失败: err=%v out=%+v", err, out)
	}

	// 既有的那一行应当拿到默认值。
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	cur, err := s.Query(ctx, `SELECT label FROM t WHERE id = 1`, nil, driver.QueryOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = cur.Close() }()
	rows, _, err := cur.Next(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("Next: %v rows=%v", err, rows)
	}
	if got, _ := rows[0][0].(string); got != "未命名" {
		t.Errorf("label = %q, 期望 未命名", got)
	}
}
