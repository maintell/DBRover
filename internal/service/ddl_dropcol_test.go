package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestDropColumnIsIrreversibleOnRealEngines 是 docs/07 验收第 3 条的引擎侧证据。
//
// 「删列不可回滚且丢数据」不能只写在界面上：预案本身必须说清楚——标不可逆、
// 给出现有非空行数（丢多少），回滚语句只能把列加回来。三件事缺一件，界面
// 上的红色警示就成了空话。
func TestDropColumnIsIrreversibleOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP TABLE IF EXISTS it_dropcol",
				"CREATE TABLE it_dropcol (id INT PRIMARY KEY, v VARCHAR(32), keep VARCHAR(8))",
			}, func(ctx context.Context, s driver.Session) {
				rows := [][]any{{1, "a", "k"}, {2, nil, "k"}}
				for _, row := range rows {
					if _, err := s.Execute(ctx,
						"INSERT INTO it_dropcol (id, v, keep) VALUES (?, ?, ?)", row); err != nil {
						t.Fatalf("插入数据: %v", err)
					}
				}
			})

			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table:  tableRef("it_dropcol"),
				Action: service.DDLDropColumn,
				Column: service.ColumnSpec{Name: "v"},
			})
			if err != nil {
				t.Fatalf("BuildDDLPlan(drop): %v", err)
			}
			if !plan.Irreversible {
				t.Error("删列必须标不可逆：回滚只能恢复结构，数据回不来")
			}
			if plan.AffectedRows != 1 {
				t.Errorf("AffectedRows = %d，期望 1（只有一行 v 非空，界面靠它说「会丢多少」）", plan.AffectedRows)
			}
			if len(plan.Rollback) != 1 ||
				!strings.Contains(strings.ToUpper(plan.Rollback[0].SQL), "ADD COLUMN") {
				t.Fatalf("回滚语句应当是把列加回来，实际 %+v", plan.Rollback)
			}
			// DDL 隐式提交的引擎要如实标「补偿不是撤销」，否则用户会以为能回滚。
			if plan.RollsBackAsCompensation == conn.Capabilities().DDLTransactional {
				t.Errorf("RollsBackAsCompensation = %v 与 DDLTransactional = %v 矛盾",
					plan.RollsBackAsCompensation, conn.Capabilities().DDLTransactional)
			}

			out, err := ex.ApplyChangePlan(ctx, plan, service.Request{})
			if err != nil || out.Err != nil || out.Blocked {
				t.Fatalf("应用删列失败: err=%v out=%+v", err, out)
			}
			detail, err := ex.DescribeTable(ctx, tableRef("it_dropcol"))
			if err != nil {
				t.Fatalf("DescribeTable: %v", err)
			}
			if hasColumnNamed(detail, "v") {
				t.Error("执行后列 v 应该已经不存在")
			}
			if !hasColumnNamed(detail, "keep") {
				t.Error("执行后列 keep 不该受影响")
			}
		})
	}
}
