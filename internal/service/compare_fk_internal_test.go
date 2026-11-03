package service

import (
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// fkOf 造一个外键描述，减少用例里的噪音。
func fkOf(name, table string, cols []string, refTable string, refCols []string, onDelete string) model.ForeignKey {
	return model.ForeignKey{
		Name: name, Columns: cols, RefTable: refTable, RefColumns: refCols, OnDelete: onDelete,
	}
}

// TestCompareForeignKeys 锁住外键比对的规则：
// 按「本表列 → 引用表 + 引用列」为键，不按名字；动作子句变了算同一外键有差异。
func TestCompareForeignKeys(t *testing.T) {
	base := model.ForeignKey{
		Name: "fk_orders_customer", Columns: []string{"customer_id"},
		RefTable: "customers", RefColumns: []string{"id"},
	}
	other := model.ForeignKey{
		Name: "fk_orders_customer", Columns: []string{"customer_id"},
		RefTable: "customers", RefColumns: []string{"id"}, OnDelete: "CASCADE",
	}
	extra := model.ForeignKey{
		Name: "fk_orders_agent", Columns: []string{"agent_id"},
		RefTable: "agents", RefColumns: []string{"id"},
	}

	left := &model.TableDetail{ForeignKeys: []model.ForeignKey{base}}
	right := &model.TableDetail{ForeignKeys: []model.ForeignKey{other, extra}}

	diffs := compareForeignKeys(left, right)
	if len(diffs) != 2 {
		t.Fatalf("差异数 = %d, 期望 2（一个动作变了、一个只在右侧）: %+v", len(diffs), diffs)
	}

	byKey := map[string]ForeignKeyDiff{}
	for _, d := range diffs {
		byKey[d.Key] = d
	}

	// 同一关系、不同动作：必须报成 changed，而不是「一删一增」。
	changed, ok := byKey["customer_id->customers(id)"]
	if !ok {
		t.Fatalf("没按「列 → 引用表(列)」建键: %+v", diffs)
	}
	if changed.Status != CompareChanged {
		t.Errorf("动作变更的状态 = %s, 期望 changed", changed.Status)
	}
	if len(changed.Reason) == 0 || !strings.Contains(strings.Join(changed.Reason, "；"), "ON DELETE") {
		t.Errorf("应说明是 ON DELETE 变了: %+v", changed.Reason)
	}

	onlyRight, ok := byKey["agent_id->agents(id)"]
	if !ok || onlyRight.Status != CompareOnlyRight {
		t.Errorf("右侧独有的外键应报 only_right: %+v", diffs)
	}

	// 名字不同不算差异（键里不含名字），但要说明，免得人以为漏了。
	renamed := base
	renamed.Name = "fk_whatever"
	got := compareForeignKeys(left, &model.TableDetail{ForeignKeys: []model.ForeignKey{renamed}})
	if len(got) != 1 || got[0].Status != CompareChanged {
		t.Fatalf("只改名字应报一条 changed: %+v", got)
	}
	if !strings.Contains(strings.Join(got[0].Reason, "；"), "外键名不同") {
		t.Errorf("应说明只是名字不同: %+v", got[0].Reason)
	}

	// 完全相同 → 零差异。
	if got := compareForeignKeys(left, &model.TableDetail{ForeignKeys: []model.ForeignKey{base}}); len(got) != 0 {
		t.Errorf("相同外键不该有差异: %+v", got)
	}
}

// TestCompareChecks 检查约束按归一化表达式匹配，名字不同不算差异。
func TestCompareChecks(t *testing.T) {
	left := &model.TableDetail{Checks: []model.Check{{Name: "ck_age", Expr: "age >= 0"}}}
	right := &model.TableDetail{Checks: []model.Check{{Name: "ck_age_v2", Expr: "AGE  >=  0"}}}

	got := compareChecks(left, right)
	if len(got) != 1 || got[0].Status != CompareChanged {
		t.Fatalf("仅名字不同应报一条 changed（表达式归一后相同）: %+v", got)
	}
	if !strings.Contains(strings.Join(got[0].Reason, "；"), "约束名不同") {
		t.Errorf("原因应指出是约束名: %+v", got[0].Reason)
	}

	// 表达式不同 → 两条（一左一右），因为键本身就不同。
	diffs := compareChecks(left, &model.TableDetail{Checks: []model.Check{{Name: "ck_age", Expr: "age > 0"}}})
	if len(diffs) != 2 {
		t.Fatalf("表达式不同应报一左一右两条: %+v", diffs)
	}
}
