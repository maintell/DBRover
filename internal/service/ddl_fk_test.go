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

// TestRawDDLPlan 覆盖 docs/07 §1.2 的手写 DDL 入口。
//
// 三条限制逐条断言：多语句拒绝、非 DDL 拒绝、以及「不做语义预检」必须
// 在预案里写明并标不可逆——不假装能回滚。
func TestRawDDLPlan(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `CREATE TABLE raw_probe (id INTEGER PRIMARY KEY, v TEXT)`, nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	_ = s.Close()

	// 1) 正常一条 ALTER：能出预案，标不可逆，Notes 里说明未做预检。
	plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("raw_probe"),
		Action: service.DDLRaw,
		RawSQL: "ALTER TABLE raw_probe ADD COLUMN note TEXT",
	})
	if err != nil {
		t.Fatalf("手写 DDL 应能出预案: %v", err)
	}
	if !plan.DDL || !plan.Irreversible {
		t.Errorf("手写 DDL 应标 DDL 且不可逆: %+v", plan)
	}
	if len(plan.Statements) != 1 || plan.Statements[0].ExpectRows != -1 {
		t.Errorf("应恰好一条语句且不校验行数: %+v", plan.Statements)
	}
	if len(plan.Rollback) != 0 {
		t.Errorf("手写 DDL 不该凭空生成回滚语句: %+v", plan.Rollback)
	}
	joined := strings.Join(plan.Notes, "；")
	if !strings.Contains(joined, "未做语义预检") {
		t.Errorf("必须写明未做语义预检: %q", joined)
	}
	if !strings.Contains(joined, "不生成回滚语句") {
		t.Errorf("必须写明没有回滚: %q", joined)
	}
	if plan.ExpectDDLHash == "" {
		t.Error("指明了存在的表时应有定义哈希做竞态保护")
	}

	// 2) 多条语句：拒绝（没法保证原子性）。
	if _, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("raw_probe"),
		Action: service.DDLRaw,
		RawSQL: "ALTER TABLE raw_probe ADD COLUMN a TEXT; ALTER TABLE raw_probe ADD COLUMN b TEXT",
	}); !errors.Is(err, service.ErrInvalidDDL) {
		t.Errorf("多语句应被拒, 实际 %v", err)
	} else if !strings.Contains(err.Error(), "一次只能提交一条") {
		t.Errorf("拒绝理由要说清是「一次一条」: %v", err)
	}

	// 3) 非 DDL：拒绝，并指路到变更集。
	_, err = ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("raw_probe"),
		Action: service.DDLRaw,
		RawSQL: "UPDATE raw_probe SET v = 'x'",
	})
	if !errors.Is(err, service.ErrInvalidDDL) {
		t.Fatalf("DML 应被拒, 实际 %v", err)
	}
	if !strings.Contains(err.Error(), "变更集") {
		t.Errorf("应指路到变更集: %v", err)
	}

	// 4) 空语句：拒绝。
	if _, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Action: service.DDLRaw, RawSQL: "   ",
	}); !errors.Is(err, service.ErrInvalidDDL) {
		t.Errorf("空语句应被拒, 实际 %v", err)
	}

	// 5) 执行走同一个 apply：加列成功。
	if out, err := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); err != nil || out.Err != nil || out.Blocked {
		t.Fatalf("执行手写 DDL 失败: err=%v out=%+v", err, out)
	}
	td, err := ex.DescribeTable(ctx, tableRef("raw_probe"))
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if _, ok := findColForTest(td, "note"); !ok {
		t.Error("执行后应有 note 列")
	}
}

// findColForTest 是 service_test 包里的小工具（service 内部的 findColumn 不可见）。
func findColForTest(td *model.TableDetail, name string) (model.Column, bool) {
	for _, c := range td.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return model.Column{}, false
}
