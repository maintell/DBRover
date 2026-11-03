package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/service"
)

// TestDDLDropTableBlockedByView 对应 docs/07 §5.1 与验收项 8：
// 表还被视图引用时，删表必须在生成预案阶段就被挡下来。
//
// 为什么必须在预案阶段挡：DROP TABLE 一旦执行没有回滚，等视图下次被查询
// 才报错，那时表和数据都已经没了。
func TestDDLDropTableBlockedByView(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE dep_parent (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE VIEW dep_view AS SELECT id, v FROM dep_parent`,
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("准备 %q: %v", q, err)
		}
	}
	_ = s.Close()

	_, err = ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table: tableRef("dep_parent"), Action: service.DDLDropTable,
	})
	if !errors.Is(err, service.ErrDependencyBlocked) {
		t.Fatalf("被视图引用时应阻断, 实际 %v", err)
	}
	if !strings.Contains(err.Error(), "dep_view") {
		t.Errorf("阻断信息应指出是哪个视图: %v", err)
	}
	// SQLite 的视图依赖靠文本匹配，必须标注可能误报。
	if !strings.Contains(err.Error(), "可能误报") {
		t.Errorf("近似结果必须标注: %v", err)
	}

	// 删掉视图后应放行，并且预案是「不可逆 + 补偿回滚」。
	s, err = e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `DROP VIEW dep_view`, nil); err != nil {
		t.Fatalf("删视图: %v", err)
	}
	_ = s.Close()

	plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table: tableRef("dep_parent"), Action: service.DDLDropTable,
	})
	if err != nil {
		t.Fatalf("删掉视图后应放行: %v", err)
	}
	if !plan.Irreversible {
		t.Error("删表必须标不可逆")
	}
	if len(plan.Statements) != 1 || !strings.HasPrefix(plan.Statements[0].SQL, "DROP TABLE") {
		t.Fatalf("正向语句不对: %+v", plan.Statements)
	}
	if len(plan.Rollback) != 1 || !strings.Contains(plan.Rollback[0].SQL, "CREATE TABLE") {
		t.Fatalf("应给出补偿用的建表语句: %+v", plan.Rollback)
	}
	if len(plan.Notes) == 0 {
		t.Error("删表必须给一条「数据不会回来」的说明")
	}

	// 执行后表确实没了；补偿语句能把空表建回来（数据不回来）。
	if out, err := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("执行删表失败: err=%v out=%+v", err, out)
	}
	if _, err := ex.DescribeTable(ctx, tableRef("dep_parent")); err == nil {
		t.Fatal("执行后表应已删除")
	}
	undo := &service.ChangePlan{
		ID: "undo-droptable", Table: tableRef("dep_parent"), Statements: plan.Rollback,
		Risk: plan.Risk, Friction: plan.Friction, ExpiresAt: plan.ExpiresAt,
	}
	if out, err := ex.ApplyChangePlan(ctx, undo, service.Request{Approval: true}); err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("补偿建表失败: err=%v out=%+v", err, out)
	}
	td, err := ex.DescribeTable(ctx, tableRef("dep_parent"))
	if err != nil {
		t.Fatalf("补偿后应能读到表: %v", err)
	}
	if len(td.Columns) != 2 {
		t.Errorf("补偿后的列数 = %d, 期望 2", len(td.Columns))
	}
	if len(td.PrimaryKeyColumns()) != 1 {
		t.Errorf("补偿后应保留主键, 实际 %v", td.PrimaryKeyColumns())
	}
}

// TestDDLDropColumnBlockedByForeignKey 验证删列时的精确阻断：
// 被外键引用到的那一列不能删，没被引用的列可以删。
func TestDDLDropColumnBlockedByForeignKey(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE fkp (id INTEGER PRIMARY KEY, code TEXT UNIQUE, note TEXT)`,
		`CREATE TABLE fkc (id INTEGER PRIMARY KEY, pcode TEXT REFERENCES fkp(code))`,
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("准备 %q: %v", q, err)
		}
	}
	_ = s.Close()

	// code 被 fkc.pcode 引用 → 阻断。
	_, err = ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table: tableRef("fkp"), Action: service.DDLDropColumn,
		Column: service.ColumnSpec{Name: "code"},
	})
	if !errors.Is(err, service.ErrDependencyBlocked) {
		t.Fatalf("被外键引用的列不能删, 实际 %v", err)
	}
	if !strings.Contains(err.Error(), "fkc") {
		t.Errorf("阻断信息应指出引用方: %v", err)
	}
	if strings.Contains(err.Error(), "可能误报") {
		t.Errorf("外键依赖是精确的，不该标近似: %v", err)
	}

	// note 没被引用 → 放行。
	if _, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table: tableRef("fkp"), Action: service.DDLDropColumn,
		Column: service.ColumnSpec{Name: "note"},
	}); err != nil {
		t.Fatalf("没被引用的列应能删: %v", err)
	}
}
