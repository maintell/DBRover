package service

import (
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestAnalyzePlanLargeScanIsCrit 锁住「行数大才升红」（docs/05 §4.2 第 2 条）。
//
// 两个方向都要断言：大表全表扫描必须是 crit（否则用户照旧不看），小表全表扫描
// 必须留在 warn——把小表也标红，红色就变成了背景色，等于没有。
func TestAnalyzePlanLargeScanIsCrit(t *testing.T) {
	pgCols := []model.Column{{Name: "QUERY PLAN"}}

	t.Run("PG 估算行数过万", func(t *testing.T) {
		got := AnalyzePlan(model.EnginePostgres, pgCols, [][]any{
			{"Seq Scan on orders  (cost=0.00..35850.00 rows=1200000 width=8)"},
		})
		if len(got) != 1 || got[0].Code != "FULL_SCAN" {
			t.Fatalf("应报一条全表扫描，实际 %+v", got)
		}
		if got[0].Severity != "crit" {
			t.Errorf("估算 120 万行的全表扫描应是 crit，实际 %q", got[0].Severity)
		}
		if !strings.Contains(got[0].Message, "估算") {
			t.Errorf("结论必须说明行数是估算，实际 %q", got[0].Message)
		}
	})

	t.Run("PG 小表仍是 warn", func(t *testing.T) {
		got := AnalyzePlan(model.EnginePostgres, pgCols, [][]any{
			{"Seq Scan on tiny  (cost=0.00..1.20 rows=20 width=8)"},
		})
		if len(got) != 1 || got[0].Severity != "warn" {
			t.Fatalf("小表全表扫描应是 warn，实际 %+v", got)
		}
	})

	t.Run("MySQL 估算行数过万", func(t *testing.T) {
		cols := []model.Column{{Name: "id"}, {Name: "table"}, {Name: "type"}, {Name: "rows"}, {Name: "Extra"}}
		got := AnalyzePlan(model.EngineMySQL, cols, [][]any{
			{int64(1), "orders", "ALL", int64(800000), ""},
		})
		if len(got) != 1 || got[0].Code != "FULL_SCAN" || got[0].Severity != "crit" {
			t.Fatalf("80 万行的 type=ALL 应是 crit 的 FULL_SCAN，实际 %+v", got)
		}
		if !strings.Contains(got[0].Message, "800000") {
			t.Errorf("结论里应带上估算行数，实际 %q", got[0].Message)
		}
	})

	t.Run("MySQL 小表 type=ALL 仍是 warn", func(t *testing.T) {
		cols := []model.Column{{Name: "id"}, {Name: "table"}, {Name: "type"}, {Name: "rows"}, {Name: "Extra"}}
		got := AnalyzePlan(model.EngineMySQL, cols, [][]any{
			{int64(1), "cfg", "ALL", int64(3), ""},
		})
		if len(got) != 1 || got[0].Severity != "warn" {
			t.Fatalf("3 行的全表扫描应是 warn，实际 %+v", got)
		}
	})
}

// TestPlanRowEstimateParsesPGText 钉住那一个数字的解析：取不到就当未知（0），
// 不对未知的量下判断。
func TestPlanRowEstimateParsesPGText(t *testing.T) {
	cases := map[string]int64{
		"Nested Loop  (cost=0.29..8.30 rows=1 width=8)":         1,
		"Seq Scan on t  (cost=0.00..155.00 rows=12000 width=4)": 12000,
		"Seq Scan on t  (cost=0.00..155.00 width=4)":            0,
		"": 0,
	}
	for line, want := range cases {
		if got := planRowEstimate(line); got != want {
			t.Errorf("planRowEstimate(%q) = %d，期望 %d", line, got, want)
		}
	}
}
