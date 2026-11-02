package service_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
	"github.com/maintell/DBRover/internal/service"
)

type env struct {
	rw  *sqlite.Connector
	ro  *sqlite.Connector
	dir string
}

// setup 建一个已经有一行数据的库，并同时提供读写与只读两个连接器。
func setup(t *testing.T) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "svc.db")
	ctx := context.Background()

	rw := sqlite.New()
	if err := rw.Open(ctx, driver.ConnectConfig{
		Host: path, AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
	}); err != nil {
		t.Fatalf("打开读写连接: %v", err)
	}
	t.Cleanup(func() { _ = rw.Close() })

	s, err := rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`, nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	if _, err := s.Execute(ctx, `INSERT INTO t (id, v) VALUES (1, 'a')`, nil); err != nil {
		t.Fatalf("插数据: %v", err)
	}
	_ = s.Close()

	ro := sqlite.New()
	if err := ro.Open(ctx, driver.ConnectConfig{
		Host: path, AccessMode: model.AccessReadOnly, Environment: model.EnvTest,
	}); err != nil {
		t.Fatalf("打开只读连接: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	return &env{rw: rw, ro: ro, dir: path}
}

func (e *env) value(t *testing.T, id int64) string {
	t.Helper()
	ctx := context.Background()
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()

	cur, err := s.Query(ctx, `SELECT v FROM t WHERE id = ?`, []any{id}, driver.QueryOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = cur.Close() }()
	rows, _, err := cur.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("id=%d 不存在", id)
	}
	s2, _ := rows[0][0].(string)
	return s2
}

func TestReadOnlyConnectionBlocksWrite(t *testing.T) {
	e := setup(t)
	out, err := service.NewExecutor(e.ro, model.EnvTest, model.AccessReadOnly, nil).
		Execute(context.Background(), service.Request{SQL: `UPDATE t SET v='x' WHERE id=1`})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Blocked {
		t.Fatal("只读连接上的 UPDATE 应被拒绝")
	}
	if out.Friction != policy.F5 {
		t.Errorf("Friction = %s, 期望 F5", out.Friction)
	}
	if !strings.Contains(out.Reason, "只读") {
		t.Errorf("Reason 未说明原因: %q", out.Reason)
	}
}

func TestTestEnvSingleRowUpdateNeedsNoApproval(t *testing.T) {
	e := setup(t)
	out, err := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil).
		Execute(context.Background(), service.Request{SQL: `UPDATE t SET v='b' WHERE id=1`})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Blocked {
		t.Fatalf("测试环境的单行更新不应被拦截: %s", out.Reason)
	}
	if out.Risk != policy.R1 {
		t.Errorf("Risk = %s, 期望 R1（命中主键）", out.Risk)
	}
	if got := e.value(t, 1); got != "b" {
		t.Errorf("v = %q, 期望 b", got)
	}
}

func TestProductionBatchUpdateRequiresApproval(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvProduction, model.AccessReadWrite, nil)

	out, err := ex.Execute(context.Background(), service.Request{SQL: `UPDATE t SET v='c' WHERE id > 0`})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Blocked {
		t.Fatal("生产环境的 R2 操作未审批时应被拦截")
	}
	if out.Risk != policy.R2 {
		t.Errorf("Risk = %s, 期望 R2", out.Risk)
	}
	if got := e.value(t, 1); got != "a" {
		t.Errorf("被拦截后数据不应变化, 实际 v = %q", got)
	}

	out2, err := ex.Execute(context.Background(), service.Request{SQL: `UPDATE t SET v='c' WHERE id > 0`, Approval: true})
	if err != nil {
		t.Fatalf("Execute(approve): %v", err)
	}
	if out2.Blocked {
		t.Fatalf("已审批仍被拦截: %s", out2.Reason)
	}
	if got := e.value(t, 1); got != "c" {
		t.Errorf("审批后应生效, 实际 v = %q", got)
	}
}

func TestUnknownEnvBehavesLikeProduction(t *testing.T) {
	e := setup(t)
	out, err := service.NewExecutor(e.rw, model.EnvUnknown, model.AccessReadWrite, nil).
		Execute(context.Background(), service.Request{SQL: `UPDATE t SET v='z' WHERE id > 0`})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Blocked {
		t.Fatal("unknown 环境应按生产严格度拦截")
	}
}

func TestProductionDDLRequiresApproval(t *testing.T) {
	e := setup(t)
	out, err := service.NewExecutor(e.rw, model.EnvProduction, model.AccessReadWrite, nil).
		Execute(context.Background(), service.Request{SQL: `ALTER TABLE t ADD COLUMN note TEXT`})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Blocked {
		t.Fatal("生产环境 DDL 未审批时应被拦截")
	}
	if out.Risk != policy.R3 {
		t.Errorf("Risk = %s, 期望 R3", out.Risk)
	}
}

func TestLocalEnvDDLRunsWithoutApproval(t *testing.T) {
	e := setup(t)
	out, err := service.NewExecutor(e.rw, model.EnvLocal, model.AccessReadWrite, nil).
		Execute(context.Background(), service.Request{SQL: `ALTER TABLE t ADD COLUMN note TEXT`})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Blocked {
		t.Fatalf("本地环境的 DDL 不应被拦截: %s", out.Reason)
	}
}

// TestBatchIsBlockedAtomically 验证「任一条被拒则整批不执行」。
func TestBatchIsBlockedAtomically(t *testing.T) {
	e := setup(t)
	sqlText := `UPDATE t SET v='p1' WHERE id=1;
DELETE FROM t WHERE id > 100;`
	out, err := service.NewExecutor(e.rw, model.EnvProduction, model.AccessReadWrite, nil).
		Execute(context.Background(), service.Request{SQL: sqlText})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Blocked {
		t.Fatal("批次内含 R4 语句时整批应被拒绝")
	}
	if got := e.value(t, 1); got != "a" {
		t.Errorf("整批拒绝后第一条也不应生效, 实际 v = %q", got)
	}
}

func TestSplitErrorIsDenied(t *testing.T) {
	e := setup(t)
	out, err := service.NewExecutor(e.rw, model.EnvLocal, model.AccessReadWrite, nil).
		Execute(context.Background(), service.Request{SQL: `SELECT 'abc`})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Blocked {
		t.Fatal("无法切分的脚本应被拒绝")
	}
	if out.Risk != policy.R4 {
		t.Errorf("Risk = %s, 期望 R4", out.Risk)
	}
}

func TestQueryReturnsColumnsAndRows(t *testing.T) {
	e := setup(t)
	out, err := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil).
		Execute(context.Background(), service.Request{SQL: `SELECT id, v FROM t ORDER BY id`})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Blocked {
		t.Fatalf("查询不应被拦截: %s", out.Reason)
	}
	if len(out.Results) != 1 {
		t.Fatalf("结果数 = %d", len(out.Results))
	}
	r := out.Results[0]
	if len(r.Columns) != 2 {
		t.Fatalf("列数 = %d", len(r.Columns))
	}
	if len(r.Rows) != 1 {
		t.Fatalf("行数 = %d", len(r.Rows))
	}
	if r.Grade.Level != policy.R0 {
		t.Errorf("Level = %s, 期望 R0", r.Grade.Level)
	}
}

func TestAuditRecordsBothAllowedAndDenied(t *testing.T) {
	e := setup(t)
	audit := &service.SliceAudit{}
	ex := service.NewExecutor(e.rw, model.EnvProduction, model.AccessReadWrite, audit)

	if _, err := ex.Execute(context.Background(), service.Request{SQL: `SELECT 1`}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := ex.Execute(context.Background(), service.Request{SQL: `DELETE FROM t`}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(audit.Events) != 2 {
		t.Fatalf("审计条数 = %d, 期望 2", len(audit.Events))
	}
	if audit.Events[0].Outcome != "ok" {
		t.Errorf("第一条 outcome = %q, 期望 ok", audit.Events[0].Outcome)
	}
	if audit.Events[1].Outcome != "denied" {
		t.Errorf("第二条 outcome = %q, 期望 denied", audit.Events[1].Outcome)
	}
	if audit.Events[1].Risk != policy.R4 {
		t.Errorf("DELETE 无 WHERE 应为 R4, 实际 %s", audit.Events[1].Risk)
	}
}

func TestQueryLimitRespected(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for i := 2; i <= 30; i++ {
		if _, err := s.Execute(ctx, `INSERT INTO t (id, v) VALUES (?, 'x')`, []any{i}); err != nil {
			t.Fatalf("插入: %v", err)
		}
	}
	_ = s.Close()

	out, err := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil).
		Execute(ctx, service.Request{SQL: `SELECT id FROM t ORDER BY id`, MaxRows: 10})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := len(out.Results[0].Rows); got != 10 {
		t.Errorf("行数 = %d, 期望 10（MaxRows 生效）", got)
	}
}
