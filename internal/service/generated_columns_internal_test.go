package service

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver/dialect"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
)

// setupGeneratedColumnDB 建一张带生成列的表：a 是普通列，b = a*2（存储型生成列）。
// SQLite 只在 CREATE TABLE 里接受生成列，所以这里一次建好。
func setupGeneratedColumnDB(t *testing.T) *sqlite.Connector {
	t.Helper()
	c := openEmptySQLite(t)
	ctx := context.Background()
	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Execute(ctx,
		"CREATE TABLE g (id INTEGER PRIMARY KEY, a INTEGER, b INTEGER GENERATED ALWAYS AS (a * 2) STORED)",
		nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	if _, err := s.Execute(ctx, "INSERT INTO g (id, a) VALUES (1, 5)", nil); err != nil {
		t.Fatalf("插数据: %v", err)
	}
	return c
}

// TestDescribeReportsGeneratedColumn 锁住「生成列必须出现在列清单里」。
//
// 用 PRAGMA table_info 时它根本不返回生成列：结构页看不到、导出不知道它存在，
// 等于静默漏列。改成 table_xinfo 之后还要确认 hidden=2/3 被标成 IsGenerated，
// 否则下一个调用方仍会把它当普通列写进 INSERT。
func TestDescribeReportsGeneratedColumn(t *testing.T) {
	ctx := context.Background()
	db := setupGeneratedColumnDB(t)
	ex := NewExecutor(db, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	td, err := ex.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: "g"})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if len(td.Columns) != 3 {
		t.Fatalf("列数 = %d，期望 3（含生成列 b）：%+v", len(td.Columns), td.Columns)
	}
	byName := map[string]model.Column{}
	for _, c := range td.Columns {
		byName[c.Name] = c
	}
	if !byName["b"].IsGenerated {
		t.Errorf("b 应标为生成列，实际 %+v", byName["b"])
	}
	if byName["a"].IsGenerated {
		t.Errorf("a 不是生成列，却被标记了")
	}
}

// TestExportSQLSkipsGeneratedColumnButKeepsData 两条同时成立：
//  1. INSERT 列清单里没有生成列（写进去引擎会直接拒绝）；
//  2. 回放之后 b 的值仍然正确（由目标端按表达式算出来）——数据没丢。
func TestExportSQLSkipsGeneratedColumnButKeepsData(t *testing.T) {
	ctx := context.Background()
	db := setupGeneratedColumnDB(t)
	ex := NewExecutor(db, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	var buf bytes.Buffer
	if _, err := ex.Export(ctx, ExportRequest{Table: "g", Format: ExportSQL}, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	sqlText := buf.String()
	for _, line := range strings.Split(sqlText, "\n") {
		if strings.HasPrefix(line, "INSERT INTO") && strings.Contains(line, "\"b\"") {
			t.Fatalf("INSERT 里出现了生成列 b：%s", line)
		}
	}
	if !strings.Contains(sqlText, "生成列 b 不写入") {
		t.Errorf("导出头应说明生成列不写入，实际开头：%s", firstLines(sqlText, 4))
	}

	// 回放到空库：用同一条切分路径，避免手写分号规则。
	dst := openEmptySQLite(t)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer dstEx.Close()
	sess, err := dst.Acquire(ctx)
	if err != nil {
		t.Fatalf("目标 Acquire: %v", err)
	}
	defer func() { _ = sess.Close() }()
	stmts, err := dialect.For(model.EngineSQLite).Split(sqlText)
	if err != nil {
		t.Fatalf("切分导出脚本: %v", err)
	}
	for _, st := range stmts {
		if _, err := sess.Execute(ctx, st.Text, nil); err != nil {
			t.Fatalf("回放失败（%s）：%v", st.Text, err)
		}
	}

	out, err := dstEx.ReadOnlyQuery(ctx, Request{SQL: "SELECT id, a, b FROM g"})
	if err != nil {
		t.Fatalf("复查: %v", err)
	}
	row := out.Results[0].Rows[0]
	if n, _ := row[2].(int64); n != 10 {
		t.Fatalf("回放后 b = %v，期望 10（a*2 由目标端算出）", row[2])
	}
}

// TestMigrateBlocksGeneratedColumn 锁住「生成列默认不许迁」。
//
// 直接迁的后果不是报错，而是目标端多出一个空列：源里算好的值变 NULL，而摘要
// 校验两端用同一份列清单，发现不了。必须由人决定「固化成一个普通列」。
func TestMigrateBlocksGeneratedColumn(t *testing.T) {
	ctx := context.Background()
	db := setupGeneratedColumnDB(t)
	ex := NewExecutor(db, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	td, err := ex.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: "g"})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	plan := PlanMigration(td, nil, model.EngineSQLite, model.EngineSQLite)
	if plan.Blocked != 1 {
		t.Fatalf("blocked = %d，期望 1（生成列）：%+v", plan.Blocked, plan.Columns)
	}
	var got *ColumnVerdict
	for i := range plan.Columns {
		if plan.Columns[i].Column == "b" {
			got = &plan.Columns[i]
		}
	}
	if got == nil || got.Status != MigrateBlocked {
		t.Fatalf("b 的结论 = %+v，期望 blocked", got)
	}
	if !strings.Contains(got.Reason, "生成列") || !strings.Contains(got.Reason, "目标类型") {
		t.Errorf("理由要说明是什么、以及怎么继续，实际 %q", got.Reason)
	}

	// 给了决定就应当放过（把派生值固化成普通列）——否则这条路彻底走不通。
	if _, err := BuildMigrationDDL(model.EngineSQLite, "g2", td, plan, map[string]string{"b": "INTEGER"}); err != nil {
		t.Fatalf("给出目标类型后应能生成 DDL，实际 %v", err)
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " / ")
}
