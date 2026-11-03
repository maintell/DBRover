package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/service"
)

// seedFKTables 造一对父子表：parent(id PK, code UNIQUE) ← child(pid, pcode)。
func seedFKTables(t *testing.T, e *env) {
	t.Helper()
	ctx := context.Background()
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	for _, q := range []string{
		`CREATE TABLE parent (id INTEGER PRIMARY KEY, code TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX ux_parent_code ON parent (code)`,
		`CREATE TABLE child (cid INTEGER PRIMARY KEY, pid INTEGER, pcode TEXT)`,
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("建表 %s: %v", q, err)
		}
	}
}

func TestFKPrecheckCleanData(t *testing.T) {
	e := setup(t)
	seedFKTables(t, e)
	ctx := context.Background()

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO parent (id, code) VALUES (1, 'A'), (2, 'B')`,
		`INSERT INTO child (cid, pid, pcode) VALUES (10, 1, 'A'), (11, NULL, NULL)`,
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("插数据: %v", err)
		}
	}
	_ = s.Close()

	ex := newTestExecutor(t, e)
	res, err := ex.PrecheckForeignKey(ctx, service.FKPrecheckRequest{
		Child:      tableRef("child"),
		Parent:     tableRef("parent"),
		Columns:    []string{"pid"},
		RefColumns: []string{"id"},
	})
	if err != nil {
		t.Fatalf("PrecheckForeignKey: %v", err)
	}
	if res.Orphans != 0 {
		t.Errorf("孤儿行数 = %d, 期望 0（NULL 不算孤儿）", res.Orphans)
	}
	if !res.ParentUnique {
		t.Error("父表 id 是主键，应判为唯一")
	}
	if res.Blocked {
		t.Errorf("干净数据不该阻断: %s", res.Reason)
	}
}

// TestFKPrecheckOrphansBlocked 是验收项 5 的核心：孤儿数据必须被检出并阻断，且带样本。
func TestFKPrecheckOrphansBlocked(t *testing.T) {
	e := setup(t)
	seedFKTables(t, e)
	ctx := context.Background()

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO parent (id, code) VALUES (1, 'A')`,
		`INSERT INTO child (cid, pid, pcode) VALUES (10, 1, 'A'), (11, 99, 'Z'), (12, 98, 'Y')`,
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("插数据: %v", err)
		}
	}
	_ = s.Close()

	ex := newTestExecutor(t, e)
	res, err := ex.PrecheckForeignKey(ctx, service.FKPrecheckRequest{
		Child:      tableRef("child"),
		Parent:     tableRef("parent"),
		Columns:    []string{"pid"},
		RefColumns: []string{"id"},
	})
	if err != nil {
		t.Fatalf("PrecheckForeignKey: %v", err)
	}
	if res.Orphans != 2 {
		t.Fatalf("孤儿行数 = %d, 期望 2", res.Orphans)
	}
	if !res.Blocked {
		t.Error("有孤儿数据必须阻断")
	}
	if len(res.Samples) != 2 {
		t.Errorf("样本数 = %d, 期望 2", len(res.Samples))
	}
	joined := strings.Join(res.Samples, " ")
	if !strings.Contains(joined, "99") || !strings.Contains(joined, "98") {
		t.Errorf("样本应包含孤儿的主键值, 实际 %v", res.Samples)
	}
}

// TestFKPrecheckCompositeUniqueness 验证唯一性判断是按「列集合」而不是按单列：
// 只有 (code) 的唯一索引，不能证明 (id, code) 唯一。
func TestFKPrecheckCompositeUniqueness(t *testing.T) {
	e := setup(t)
	seedFKTables(t, e)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	res, err := ex.PrecheckForeignKey(ctx, service.FKPrecheckRequest{
		Child:      tableRef("child"),
		Parent:     tableRef("parent"),
		Columns:    []string{"pid", "pcode"},
		RefColumns: []string{"id", "code"},
	})
	if err != nil {
		t.Fatalf("PrecheckForeignKey: %v", err)
	}
	if res.ParentUnique {
		t.Error("(id, code) 没有对应的复合唯一约束，不该判为唯一")
	}
	// 不唯一不等于阻断：约束能建，但语义可疑，由调用方提示。
	if res.Blocked {
		t.Errorf("父表不唯一但无孤儿时不该阻断: %s", res.Reason)
	}
	if !strings.Contains(res.Reason, "不唯一") {
		t.Errorf("应提示唯一性问题, 实际 %q", res.Reason)
	}

	// 单列 code 上有唯一索引，应判为唯一。
	res, err = ex.PrecheckForeignKey(ctx, service.FKPrecheckRequest{
		Child:      tableRef("child"),
		Parent:     tableRef("parent"),
		Columns:    []string{"pcode"},
		RefColumns: []string{"code"},
	})
	if err != nil {
		t.Fatalf("PrecheckForeignKey: %v", err)
	}
	if !res.ParentUnique {
		t.Error("code 上有唯一索引，应判为唯一")
	}
}

func TestFKPrecheckRejectsBadInput(t *testing.T) {
	e := setup(t)
	seedFKTables(t, e)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	cases := []struct {
		name string
		req  service.FKPrecheckRequest
	}{
		{"没有列", service.FKPrecheckRequest{Child: tableRef("child"), Parent: tableRef("parent")}},
		{"列数不一致", service.FKPrecheckRequest{
			Child: tableRef("child"), Parent: tableRef("parent"),
			Columns: []string{"pid"}, RefColumns: []string{"id", "code"},
		}},
		{"子表列不存在", service.FKPrecheckRequest{
			Child: tableRef("child"), Parent: tableRef("parent"),
			Columns: []string{"nope"}, RefColumns: []string{"id"},
		}},
		{"父表列不存在", service.FKPrecheckRequest{
			Child: tableRef("child"), Parent: tableRef("parent"),
			Columns: []string{"pid"}, RefColumns: []string{"nope"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ex.PrecheckForeignKey(ctx, c.req); !errors.Is(err, service.ErrInvalidDDL) {
				t.Fatalf("应返回 ErrInvalidDDL, 实际 %v", err)
			}
		})
	}
}
