package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// indexNames 读回表的索引名集合。
func indexNames(t *testing.T, e *env, table string) map[string]model.Index {
	t.Helper()
	detail, err := e.rw.Meta().DescribeTable(context.Background(), tableRef(table))
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	out := map[string]model.Index{}
	for _, ix := range detail.Indexes {
		out[strings.ToLower(ix.Name)] = ix
	}
	return out
}

func TestDDLCreateAndDropIndex(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLCreateIndex,
		Index:  service.IndexSpec{Name: "ix_t_v", Columns: []string{"v"}},
	})
	if err != nil {
		t.Fatalf("BuildDDLPlan(create_index): %v", err)
	}
	if !strings.HasPrefix(plan.Statements[0].SQL, "CREATE INDEX ") {
		t.Errorf("语句不是建索引: %s", plan.Statements[0].SQL)
	}
	if !strings.Contains(plan.Rollback[0].SQL, "DROP INDEX") {
		t.Errorf("回滚语句不对: %s", plan.Rollback[0].SQL)
	}
	if out, err := ex.ApplyChangePlan(ctx, plan, service.Request{}); err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("应用建索引失败: err=%v out=%+v", err, out)
	}
	if _, ok := indexNames(t, e, "t")["ix_t_v"]; !ok {
		t.Fatal("建索引后看不到 ix_t_v")
	}

	// 同名索引不能重复建。
	if _, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLCreateIndex,
		Index:  service.IndexSpec{Name: "ix_t_v", Columns: []string{"v"}},
	}); !errors.Is(err, service.ErrInvalidDDL) {
		t.Errorf("重复建同名索引应报 ErrInvalidDDL, 实际 %v", err)
	}

	// 删索引：语句能被引擎接受（不同引擎 DROP INDEX 写法不同，由方言决定）。
	plan, err = ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLDropIndex,
		Index:  service.IndexSpec{Name: "ix_t_v"},
	})
	if err != nil {
		t.Fatalf("BuildDDLPlan(drop_index): %v", err)
	}
	if out, err := ex.ApplyChangePlan(ctx, plan, service.Request{}); err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("应用删索引失败: err=%v out=%+v", err, out)
	}
	if _, ok := indexNames(t, e, "t")["ix_t_v"]; ok {
		t.Fatal("删索引后 ix_t_v 仍在")
	}

	// 回滚语句能把索引按原定义建回来。
	if out, err := ex.ApplyChangePlan(ctx, &service.ChangePlan{
		ID:         "plan-rollback-test",
		Table:      tableRef("t"),
		Statements: plan.Rollback,
		Risk:       plan.Risk,
		Friction:   plan.Friction,
		ExpiresAt:  plan.ExpiresAt,
	}, service.Request{}); err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("回滚建索引失败: err=%v out=%+v", err, out)
	}
	if _, ok := indexNames(t, e, "t")["ix_t_v"]; !ok {
		t.Fatal("回滚后 ix_t_v 没建回来")
	}
}

// TestDDLUniqueIndexBlocksDuplicates 对应 docs/07 §5.2 的「加唯一索引前预检重复值」。
func TestDDLUniqueIndexBlocksDuplicates(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO t (id, v) VALUES (2, 'dup'), (3, 'dup')`,
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("准备重复数据: %v", err)
		}
	}
	_ = s.Close()

	_, err = ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLCreateIndex,
		Index:  service.IndexSpec{Name: "ux_t_v", Columns: []string{"v"}, Unique: true},
	})
	if !errors.Is(err, service.ErrDuplicateRows) {
		t.Fatalf("唯一索引前应发现重复值, 实际 %v", err)
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("错误信息应带样本值, 实际: %v", err)
	}

	// 非唯一索引不受影响。
	if _, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLCreateIndex,
		Index:  service.IndexSpec{Name: "ix_t_v2", Columns: []string{"v"}},
	}); err != nil {
		t.Fatalf("非唯一索引不该被拦: %v", err)
	}
}

// TestDDLUniqueIndexAllowsMultipleNulls 验证「多个 NULL 不算重复」：
// PG 与 MySQL 的唯一索引都允许多个 NULL，误判会让用户白白放弃约束。
func TestDDLUniqueIndexAllowsMultipleNulls(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `INSERT INTO t (id, v) VALUES (2, NULL), (3, NULL)`, nil); err != nil {
		t.Fatalf("准备 NULL: %v", err)
	}
	_ = s.Close()

	plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLCreateIndex,
		Index:  service.IndexSpec{Name: "ux_t_nullable", Columns: []string{"v"}, Unique: true},
	})
	if err != nil {
		t.Fatalf("多行 NULL 不该被当成重复: %v", err)
	}
	if out, err := ex.ApplyChangePlan(ctx, plan, service.Request{}); err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("应用唯一索引失败: err=%v out=%+v", err, out)
	}
}

func TestDDLIndexRejectsBadInput(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	cases := []struct {
		name string
		req  service.DDLRequest
	}{
		{"索引名带引号", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLCreateIndex,
			Index: service.IndexSpec{Name: `a"b`, Columns: []string{"v"}},
		}},
		{"没有列", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLCreateIndex,
			Index: service.IndexSpec{Name: "ix"},
		}},
		{"列不存在", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLCreateIndex,
			Index: service.IndexSpec{Name: "ix", Columns: []string{"nope"}},
		}},
		{"列重复", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLCreateIndex,
			Index: service.IndexSpec{Name: "ix", Columns: []string{"v", "V"}},
		}},
		{"删不存在的索引", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLDropIndex,
			Index: service.IndexSpec{Name: "nope"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ex.BuildDDLPlan(ctx, c.req); !errors.Is(err, service.ErrInvalidDDL) {
				t.Fatalf("应返回 ErrInvalidDDL, 实际 %v", err)
			}
		})
	}
}

// TestDDLDropPrimaryIndexRejected 验证主键索引不能被当成普通索引删掉。
func TestDDLDropPrimaryIndexRejected(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	detail, err := ex.DescribeTable(ctx, tableRef("t"))
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	var pkIndex string
	for _, ix := range detail.Indexes {
		if ix.Primary {
			pkIndex = ix.Name
		}
	}
	if pkIndex == "" {
		t.Skip("该引擎未报告主键索引名")
	}
	if _, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLDropIndex,
		Index:  service.IndexSpec{Name: pkIndex},
	}); !errors.Is(err, service.ErrInvalidDDL) {
		t.Fatalf("删主键索引应被拒, 实际 %v", err)
	}
}
