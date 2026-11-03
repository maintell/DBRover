package service

import (
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

func planCols(names ...string) []model.Column {
	out := make([]model.Column, len(names))
	for i, n := range names {
		out[i] = model.Column{Name: n}
	}
	return out
}

// TestAnalyzePlanSQLite 直接拿 SQLite 的 EXPLAIN QUERY PLAN 形状做输入：
// id / parent / notused / detail。detail 首词是 SCAN 就是全表扫描。
func TestAnalyzePlanSQLite(t *testing.T) {
	cols := planCols("id", "parent", "notused", "detail")

	full := AnalyzePlan(model.EngineSQLite, cols, [][]any{
		{int64(2), int64(0), int64(0), "SCAN items"},
		{int64(3), int64(0), int64(0), "USE TEMP B-TREE FOR ORDER BY"},
	})
	codes := map[string]bool{}
	for _, f := range full {
		codes[f.Code] = true
	}
	if !codes["FULL_SCAN"] {
		t.Errorf("SCAN 应报全表扫描: %+v", full)
	}
	if !codes["TEMP_TABLE"] {
		t.Errorf("USE TEMP B-TREE 应报临时树: %+v", full)
	}

	// 走索引的计划不该报任何东西。
	clean := AnalyzePlan(model.EngineSQLite, cols, [][]any{
		{int64(2), int64(0), int64(0), "SEARCH items USING INTEGER PRIMARY KEY (rowid=?)"},
	})
	if len(clean) != 0 {
		t.Errorf("命中索引的计划不该报偏差: %+v", clean)
	}
}

// TestAnalyzePlanPostgres PostgreSQL 的计划是纯文本行。
func TestAnalyzePlanPostgres(t *testing.T) {
	cols := planCols("QUERY PLAN")
	got := AnalyzePlan(model.EnginePostgres, cols, [][]any{
		{"Seq Scan on orders  (cost=0.00..35.50 rows=2550 width=4)"},
		{"  Filter: (status = 'paid'::text)"},
	})
	if len(got) != 1 || got[0].Code != "FULL_SCAN" {
		t.Fatalf("应只报一条全表扫描: %+v", got)
	}
	if got[0].Severity != "warn" {
		t.Errorf("全表扫描应是 warn，实际 %q", got[0].Severity)
	}

	clean := AnalyzePlan(model.EnginePostgres, cols, [][]any{
		{"Index Scan using orders_pkey on orders  (cost=0.15..8.17 rows=1 width=4)"},
	})
	if len(clean) != 0 {
		t.Errorf("走索引不该报偏差: %+v", clean)
	}
}

// TestAnalyzePlanMySQL MySQL 的计划是列式的：type=ALL 是全表扫描。
func TestAnalyzePlanMySQL(t *testing.T) {
	cols := planCols("id", "select_type", "table", "type", "possible_keys", "key", "rows", "Extra")

	got := AnalyzePlan(model.EngineMySQL, cols, [][]any{
		{int64(1), "SIMPLE", "orders", "ALL", nil, nil, int64(120000), "Using where; Using filesort"},
	})
	codes := map[string]bool{}
	for _, f := range got {
		codes[f.Code] = true
	}
	if !codes["FULL_SCAN"] {
		t.Errorf("type=ALL 应报全表扫描: %+v", got)
	}
	if !codes["FILESORT"] {
		t.Errorf("Using filesort 应报额外排序: %+v", got)
	}

	clean := AnalyzePlan(model.EngineMySQL, cols, [][]any{
		{int64(1), "SIMPLE", "orders", "const", []byte("PRIMARY"), []byte("PRIMARY"), int64(1), nil},
	})
	if len(clean) != 0 {
		t.Errorf("const 访问不该报偏差: %+v", clean)
	}
}

// TestAnalyzePlanDedupe 同一张表被扫两次只报一条，且总量有上限。
func TestAnalyzePlanDedupe(t *testing.T) {
	cols := planCols("id", "parent", "notused", "detail")
	rows := make([][]any, 0, 50)
	for i := 0; i < 50; i++ {
		rows = append(rows, []any{int64(i), int64(0), int64(0), "SCAN items"})
	}
	got := AnalyzePlan(model.EngineSQLite, cols, rows)
	if len(got) != 1 {
		t.Fatalf("重复的全表扫描应去重成 1 条，实际 %d", len(got))
	}

	// 不同表名不合并。
	rows = [][]any{
		{int64(1), int64(0), int64(0), "SCAN a"},
		{int64(2), int64(0), int64(0), "SCAN b"},
	}
	if n := len(AnalyzePlan(model.EngineSQLite, cols, rows)); n != 2 {
		t.Errorf("不同对象不该被合并，实际 %d 条", n)
	}
}

// TestAnalyzePlanIgnoresUnknownEngines 认不出的引擎不乱猜。
func TestAnalyzePlanIgnoresUnknownEngines(t *testing.T) {
	cols := planCols("detail")
	rows := [][]any{{"SCAN items"}}
	if got := AnalyzePlan(model.EngineRedis, cols, rows); len(got) != 0 {
		t.Errorf("非关系型引擎不该产出计划偏差: %+v", got)
	}
}
