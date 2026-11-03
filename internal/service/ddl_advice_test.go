package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestCreateIndexPlanGivesConcurrentlyAdvice 验收 docs/07 第 4 条。
//
// 两件事必须同时成立，缺一件这个建议就是误导：
//  1. 预案里的语句是普通 CREATE INDEX——执行门是单事务的，自动生成
//     CONCURRENTLY 会让每次执行都失败；
//  2. 提示里给出 CONCURRENTLY 的写法，并说清它不能在事务里跑、要另开
//     工作区单跑。
func TestCreateIndexPlanGivesConcurrentlyAdvice(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(pgLabelled{Connector: e.rw, engine: model.EnginePostgres},
		model.EnvTest, model.AccessReadWrite, nil)

	plan, err := ex.BuildDDLPlan(context.Background(), service.DDLRequest{
		Table:  model.ObjectRef{Kind: model.KindTable, Name: "t"},
		Action: service.DDLCreateIndex,
		Index:  service.IndexSpec{Name: "idx_v", Columns: []string{"v"}},
	})
	if err != nil {
		t.Fatalf("BuildDDLPlan: %v", err)
	}
	if len(plan.Statements) != 1 {
		t.Fatalf("语句数 = %d，期望 1", len(plan.Statements))
	}
	if strings.Contains(plan.Statements[0].SQL, "CONCURRENTLY") {
		t.Errorf("预案里不能自动生成 CONCURRENTLY（执行门是单事务的）：%s", plan.Statements[0].SQL)
	}
	notes := strings.Join(plan.Notes, "\n")
	if !strings.Contains(notes, "CONCURRENTLY") {
		t.Errorf("提示里要给出 CONCURRENTLY 的写法，实际 %q", notes)
	}
	if !strings.Contains(notes, "不能在事务里执行") {
		t.Errorf("提示里要说清不能用于事务，实际 %q", notes)
	}
	if !strings.Contains(notes, "SQL 工作区") {
		t.Errorf("提示里要指出该去哪儿跑，实际 %q", notes)
	}
}
