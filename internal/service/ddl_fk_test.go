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

// TestDDLAddForeignKeyNeedsCapability 对应 ADR-0005：
// SQLite 的 ALTER TABLE 只认 RENAME / ADD COLUMN，加约束必须重建整表。
// 所以它必须**明确拒绝**，而不是生成一条必然失败的语句。
func TestDDLAddForeignKeyNeedsCapability(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	if _, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLAddForeignKey,
		ForeignKey: service.ForeignKeySpec{
			Columns: []string{"v"}, RefTable: "t", RefColumns: []string{"id"},
		},
	}); !errors.Is(err, service.ErrInvalidDDL) {
		t.Fatalf("SQLite 上加外键应被拒绝, 实际 %v", err)
	} else if !strings.Contains(err.Error(), "不支持") {
		t.Errorf("错误应说清是引擎不支持: %v", err)
	}
}

// TestDDLAddForeignKeyOnRealEngines 在真实 MySQL / PostgreSQL 上验证加外键：
// 有孤儿数据时拒绝并给样本；数据干净时生成语句、执行成功、回滚能摘掉约束。
//
// 未设置 DBRover_TEST_MYSQL / DBRover_TEST_PG 时整组跳过。
func TestDDLAddForeignKeyOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP TABLE IF EXISTS it_fk_child",
				"DROP TABLE IF EXISTS it_fk_parent",
				"CREATE TABLE it_fk_parent (id INT NOT NULL PRIMARY KEY)",
				"CREATE TABLE it_fk_child (id INT NOT NULL PRIMARY KEY, pid INT)",
			}, func(ctx context.Context, s driver.Session) {
				if _, err := s.Execute(ctx, "INSERT INTO it_fk_parent (id) VALUES (1), (2)", nil); err != nil {
					t.Fatalf("父表造数: %v", err)
				}
				// 1 指向存在的父行，2 指向不存在的父行 → 孤儿。
				if _, err := s.Execute(ctx, "INSERT INTO it_fk_child (id, pid) VALUES (1, 1), (2, 99)", nil); err != nil {
					t.Fatalf("子表造数: %v", err)
				}
			})

			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			if !conn.Capabilities().AddForeignKey {
				t.Fatalf("%s 应支持加外键", tgt.name)
			}

			req := service.DDLRequest{
				Table:  tableRef("it_fk_child"),
				Action: service.DDLAddForeignKey,
				ForeignKey: service.ForeignKeySpec{
					Columns: []string{"pid"}, RefTable: "it_fk_parent", RefColumns: []string{"id"},
					OnDelete: "CASCADE",
				},
			}

			// 1) 有孤儿 → 阻断，并给出样本。
			_, err := ex.BuildDDLPlan(ctx, req)
			if !errors.Is(err, service.ErrForeignKeyBlocked) {
				t.Fatalf("有孤儿时应阻断, 实际 %v", err)
			}
			if !strings.Contains(err.Error(), "(99)") {
				t.Errorf("阻断信息里应带孤儿样本 (99): %v", err)
			}

			// 2) 清掉孤儿后应能出预案，语句里带动作子句与生成的名字。
			s, err := conn.Acquire(ctx)
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			if _, err := s.Execute(ctx, "DELETE FROM it_fk_child WHERE pid = 99", nil); err != nil {
				t.Fatalf("清理孤儿: %v", err)
			}
			_ = s.Close()

			plan, err := ex.BuildDDLPlan(ctx, req)
			if err != nil {
				t.Fatalf("清理后应能生成预案: %v", err)
			}
			if len(plan.Statements) != 1 || len(plan.Rollback) != 1 {
				t.Fatalf("预案应有一条正向与一条回滚: %+v", plan)
			}
			if !strings.Contains(plan.Statements[0].SQL, "FOREIGN KEY") ||
				!strings.Contains(plan.Statements[0].SQL, "CASCADE") {
				t.Errorf("语句不对: %s", plan.Statements[0].SQL)
			}
			if !strings.Contains(plan.Statements[0].SQL, "fk_it_fk_child_pid") {
				t.Errorf("应使用生成的确定名字: %s", plan.Statements[0].SQL)
			}

			// 3) 执行成功后，结构里能看到这个外键。
			if out, err := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); err != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行失败: err=%v out=%+v", err, out)
			}
			detail, err := ex.DescribeTable(ctx, tableRef("it_fk_child"))
			if err != nil {
				t.Fatalf("DescribeTable: %v", err)
			}
			if !hasFK(detail, "fk_it_fk_child_pid") {
				t.Fatalf("执行后结构里没有该外键: %+v", detail.ForeignKeys)
			}

			// 4) 回滚把它摘掉。
			undo := &service.ChangePlan{
				ID: "undo-fk", Table: tableRef("it_fk_child"), Statements: plan.Rollback,
				Risk: plan.Risk, Friction: plan.Friction, ExpiresAt: plan.ExpiresAt,
			}
			if out, err := ex.ApplyChangePlan(ctx, undo, service.Request{Approval: true}); err != nil || out.Err != nil || out.Blocked {
				t.Fatalf("回滚失败: err=%v out=%+v", err, out)
			}
			detail, err = ex.DescribeTable(ctx, tableRef("it_fk_child"))
			if err != nil {
				t.Fatalf("DescribeTable: %v", err)
			}
			if hasFK(detail, "fk_it_fk_child_pid") {
				t.Error("回滚后外键应已摘掉")
			}
		})
	}
}

func hasFK(td *model.TableDetail, name string) bool {
	for _, fk := range td.ForeignKeys {
		if strings.EqualFold(fk.Name, name) {
			return true
		}
	}
	return false
}
