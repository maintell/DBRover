package service_test

import (
	"context"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestDDLPlanOnRealEngines 验证结构变更在真实引擎上的行为差异。
//
// 关键差异在 DDL 事务性：PostgreSQL 的 DDL 参与事务、失败可整体回滚；
// MySQL 的 DDL 隐式提交，回滚语句只是补偿，界面上必须标出来。
// 这个差异由 Capabilities().DDLTransactional 声明，预案据此设置
// RollsBackAsCompensation，不允许按引擎名硬编码判断。
func TestDDLPlanOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP TABLE IF EXISTS it_ddlprobe",
				"CREATE TABLE it_ddlprobe (id INT PRIMARY KEY, v VARCHAR(32))",
			}, func(ctx context.Context, s driver.Session) {
				for i := 1; i <= 2; i++ {
					if _, err := s.Execute(ctx, "INSERT INTO it_ddlprobe (id, v) VALUES (?, ?)", []any{i, "x"}); err != nil {
						t.Fatalf("插入 id=%d: %v", i, err)
					}
				}
			})

			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			wantCompensation := !conn.Capabilities().DDLTransactional

			// 1) 加列：可回滚，回滚是删列。
			plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table:  tableRef("it_ddlprobe"),
				Action: service.DDLAddColumn,
				Column: service.ColumnSpec{Name: "note", Type: "VARCHAR(32)", Nullable: true},
			})
			if err != nil {
				t.Fatalf("BuildDDLPlan(add): %v", err)
			}
			if plan.RollsBackAsCompensation != wantCompensation {
				t.Errorf("RollsBackAsCompensation = %v, 期望 %v（cap.DDLTransactional=%v）",
					plan.RollsBackAsCompensation, wantCompensation, conn.Capabilities().DDLTransactional)
			}
			if plan.Irreversible {
				t.Error("加列不该标不可逆")
			}
			if out, err := ex.ApplyChangePlan(ctx, plan, service.Request{}); err != nil || out.Err != nil || out.Blocked {
				t.Fatalf("应用加列失败: err=%v out=%+v", err, out)
			}
			detail, err := ex.DescribeTable(ctx, tableRef("it_ddlprobe"))
			if err != nil {
				t.Fatalf("DescribeTable: %v", err)
			}
			if !hasColumnNamed(detail, "note") {
				t.Fatalf("加列后看不到 note: %v", columnList(detail))
			}

			// 2) 删列：不可逆，并给出会丢数据的非空行数。
			plan, err = ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table:  tableRef("it_ddlprobe"),
				Action: service.DDLDropColumn,
				Column: service.ColumnSpec{Name: "v"},
			})
			if err != nil {
				t.Fatalf("BuildDDLPlan(drop): %v", err)
			}
			if !plan.Irreversible {
				t.Error("删列必须标记不可逆")
			}
			if plan.AffectedRows != 2 {
				t.Errorf("非空行数 = %d, 期望 2", plan.AffectedRows)
			}
			if out, err := ex.ApplyChangePlan(ctx, plan, service.Request{}); err != nil || out.Err != nil || out.Blocked {
				t.Fatalf("应用删列失败: err=%v out=%+v", err, out)
			}
			detail, err = ex.DescribeTable(ctx, tableRef("it_ddlprobe"))
			if err != nil {
				t.Fatalf("DescribeTable: %v", err)
			}
			if hasColumnNamed(detail, "v") {
				t.Fatal("删列后 v 仍在")
			}

			// 3) 前置条件：改完结构后，基于旧结构的预案必须被拒。
			plan, err = ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table:  tableRef("it_ddlprobe"),
				Action: service.DDLAddColumn,
				Column: service.ColumnSpec{Name: "later", Type: "VARCHAR(32)", Nullable: true},
			})
			if err != nil {
				t.Fatalf("BuildDDLPlan: %v", err)
			}
			s, err := conn.Acquire(ctx)
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			if _, err := s.Execute(ctx, "ALTER TABLE it_ddlprobe ADD COLUMN other VARCHAR(32)", nil); err != nil {
				_ = s.Close()
				t.Fatalf("模拟他人改结构: %v", err)
			}
			_ = s.Close()

			out, err := ex.ApplyChangePlan(ctx, plan, service.Request{})
			if err != nil {
				t.Fatalf("ApplyChangePlan: %v", err)
			}
			if out.Err == nil || out.Err.Code != "SCHEMA_CHANGED" {
				t.Fatalf("应报 SCHEMA_CHANGED, 实际 out=%+v", out)
			}
		})
	}
}

func hasColumnNamed(detail *model.TableDetail, name string) bool {
	for _, c := range detail.Columns {
		if c.Name == name {
			return true
		}
	}
	return false
}

func columnList(detail *model.TableDetail) []string {
	out := make([]string, 0, len(detail.Columns))
	for _, c := range detail.Columns {
		out = append(out, c.Name)
	}
	return out
}
