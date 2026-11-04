package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/mysql"
	"github.com/maintell/DBRover/internal/driver/pg"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
)

func openMigrateDB(t *testing.T, name string) *sqlite.Connector {
	t.Helper()
	c := sqlite.New()
	if err := c.Open(context.Background(), driver.ConnectConfig{
		Host:        filepath.Join(t.TempDir(), name),
		AccessMode:  model.AccessReadWrite,
		Environment: model.EnvTest,
	}); err != nil {
		t.Fatalf("打开 %s: %v", name, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func execOn(t *testing.T, c *sqlite.Connector, q string) {
	t.Helper()
	s, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Execute(context.Background(), q, nil); err != nil {
		t.Fatalf("执行失败 (%s): %v", q, err)
	}
}

// TestMigrateTableWithVerification 是 ADR-0011 验证计划的第 1 条：
// 搬家后行数与摘要都要对得上，且分批次数符合预期。
func TestMigrateTableWithVerification(t *testing.T) {
	ctx := context.Background()
	src, dst := openMigrateDB(t, "src.db"), openMigrateDB(t, "dst.db")
	execOn(t, src, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execOn(t, src, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x<2500) "+
		"INSERT INTO t (id, v) SELECT x, 'v'||x FROM c")
	execOn(t, dst, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")

	srcEx := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer srcEx.Close()
	defer dstEx.Close()

	rep, err := MigrateTable(ctx, srcEx, dstEx, "t", "", MigrateExecOptions{BatchSize: 500})
	if err != nil {
		t.Fatalf("MigrateTable: %v", err)
	}
	if rep.Rows != 2500 {
		t.Fatalf("搬了 %d 行，期望 2500", rep.Rows)
	}
	if rep.Batches != 5 {
		t.Errorf("批次 = %d，期望 5（2500/500）", rep.Batches)
	}
	if !rep.KeysetOK || !rep.Verified {
		t.Fatalf("有主键的表应当支持键集且做校验：%+v", rep)
	}
	if rep.SourceDigest == "" || rep.SourceDigest != rep.TargetDigest {
		t.Fatalf("摘要不一致：%s vs %s", rep.SourceDigest, rep.TargetDigest)
	}

	// 独立复查一遍。
	if _, err := VerifyMigratedTable(ctx, srcEx, dstEx, "t", "t"); err != nil {
		t.Fatalf("复查应当通过：%v", err)
	}
}

// TestMigrateVerifyDetectsTamper 是验证计划第 2 条：改目标里 1 行必须被发现。
func TestMigrateVerifyDetectsTamper(t *testing.T) {
	ctx := context.Background()
	src, dst := openMigrateDB(t, "src.db"), openMigrateDB(t, "dst.db")
	execOn(t, src, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execOn(t, src, "INSERT INTO t (id, v) VALUES (1, 'a'), (2, 'b')")
	execOn(t, dst, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")

	srcEx := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer srcEx.Close()
	defer dstEx.Close()
	if _, err := MigrateTable(ctx, srcEx, dstEx, "t", "", MigrateExecOptions{}); err != nil {
		t.Fatalf("MigrateTable: %v", err)
	}
	execOn(t, dst, "UPDATE t SET v = 'B' WHERE id = 2") // 改一个字符

	_, err := VerifyMigratedTable(ctx, srcEx, dstEx, "t", "t")
	if !errors.Is(err, ErrMigrateVerify) {
		t.Fatalf("改了一行必须被校验发现，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "摘要") {
		t.Errorf("错误里要说明是摘要不一致，实际 %q", err.Error())
	}
}

// TestMigrateNoPrimaryKey 是验证计划第 3 条：无主键表必须明确标注两条限制。
func TestMigrateNoPrimaryKey(t *testing.T) {
	ctx := context.Background()
	src, dst := openMigrateDB(t, "src.db"), openMigrateDB(t, "dst.db")
	execOn(t, src, "CREATE TABLE n (a TEXT, b TEXT)")
	execOn(t, src, "INSERT INTO n (a, b) VALUES ('x','y'), ('1','2')")
	execOn(t, dst, "CREATE TABLE n (a TEXT, b TEXT)")

	srcEx := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer srcEx.Close()
	defer dstEx.Close()

	rep, err := MigrateTable(ctx, srcEx, dstEx, "n", "", MigrateExecOptions{})
	if err != nil {
		t.Fatalf("MigrateTable: %v", err)
	}
	if rep.KeysetOK || rep.Verified {
		t.Fatalf("无主键表不该声称可续跑或已校验：%+v", rep)
	}
	if !strings.Contains(rep.Note, "不可中断续跑") {
		t.Errorf("报告要明确写出限制，实际 %q", rep.Note)
	}
	if rep.Rows != 2 {
		t.Errorf("搬了 %d 行，期望 2", rep.Rows)
	}
}

// TestMigrateResume 是 ADR-0011 验证计划的第 1 条：中途停下后从检查点续跑，
// 最终行数与摘要都要对得上，且检查点用完即删。
func TestMigrateResume(t *testing.T) {
	ctx := context.Background()
	src, dst := openMigrateDB(t, "src.db"), openMigrateDB(t, "dst.db")
	execOn(t, src, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execOn(t, src, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x<2500) "+
		"INSERT INTO t (id, v) SELECT x, 'v'||x FROM c")
	execOn(t, dst, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	srcEx := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer srcEx.Close()
	defer dstEx.Close()

	cpPath := filepath.Join(t.TempDir(), "migrate-t.json")
	first, err := MigrateTable(ctx, srcEx, dstEx, "t", "", MigrateExecOptions{
		BatchSize: 500, MaxRows: 1000, CheckpointPath: cpPath,
	})
	if err != nil {
		t.Fatalf("首段搬运: %v", err)
	}
	if first.Rows != 1000 {
		t.Fatalf("首段应搬 1000 行，实际 %d", first.Rows)
	}
	if _, statErr := os.Stat(cpPath); statErr != nil {
		t.Fatalf("应当留下检查点：%v", statErr)
	}

	second, err := MigrateTable(ctx, srcEx, dstEx, "t", "", MigrateExecOptions{
		BatchSize: 500, CheckpointPath: cpPath, Resume: true,
	})
	if err != nil {
		t.Fatalf("续跑: %v", err)
	}
	if second.Rows != 2500 {
		t.Fatalf("续跑后总行数 = %d，期望 2500", second.Rows)
	}
	if !second.Verified || second.SourceDigest == "" || second.SourceDigest != second.TargetDigest {
		t.Fatalf("续跑后的校验不通过：%+v", second)
	}
	if _, statErr := os.Stat(cpPath); !os.IsNotExist(statErr) {
		t.Errorf("完成后检查点应被删掉，实际 %v", statErr)
	}
}

// TestMigrateResumeNeedsCheckpoint 保证没有检查点时明确报错，不静默从头
// ——从头跑会撞主键冲突，用户会误以为是坏数据。
func TestMigrateResumeNeedsCheckpoint(t *testing.T) {
	ctx := context.Background()
	src, dst := openMigrateDB(t, "src.db"), openMigrateDB(t, "dst.db")
	execOn(t, src, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	execOn(t, dst, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	srcEx := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer srcEx.Close()
	defer dstEx.Close()

	_, err := MigrateTable(ctx, srcEx, dstEx, "t", "", MigrateExecOptions{
		CheckpointPath: filepath.Join(t.TempDir(), "none.json"), Resume: true,
	})
	if !errors.Is(err, ErrMigrateNoCheckpoint) {
		t.Fatalf("缺检查点应返回 ErrMigrateNoCheckpoint，实际 %v", err)
	}
}

// TestMigrateCrossEngine 是真机跨引擎验证：MySQL 5.7 → PostgreSQL 17。
//
// 类型路径是「跨引擎 → 一律 review → 人工给 decisions」，这里显式给出目标类型，
// 然后走完整的建表 + 搬运 + 校验。它验证的是整条链路，不是某一个函数。
func TestMigrateCrossEngine(t *testing.T) {
	myDSN := os.Getenv("DBRover_TEST_MYSQL")
	pgDSN := os.Getenv("DBRover_TEST_PG")
	if myDSN == "" || pgDSN == "" {
		t.Skip("需要 DBRover_TEST_MYSQL 与 DBRover_TEST_PG")
	}
	ctx := context.Background()
	myConn := openReal(t, "mysql", myDSN)
	pgConn := openReal(t, "pg", pgDSN)
	defer func() { _ = myConn.Close() }()
	defer func() { _ = pgConn.Close() }()
	myEx := NewExecutor(myConn, model.EnvTest, model.AccessReadWrite, nil)
	pgEx := NewExecutor(pgConn, model.EnvTest, model.AccessReadWrite, nil)
	defer myEx.Close()
	defer pgEx.Close()

	// 源表：MySQL 上建好并灌数据。
	for _, q := range []string{
		"DROP TABLE IF EXISTS mig_src",
		"CREATE TABLE mig_src (id INT PRIMARY KEY, v VARCHAR(32))",
		"INSERT INTO mig_src (id, v) VALUES (1,'a'), (2,'b'), (3,'it''s ok')",
	} {
		if out, err := myEx.Execute(ctx, Request{SQL: q, Approval: true, Actor: "human:test"}); err != nil || out.Err != nil {
			t.Fatalf("准备 MySQL（%s）: err=%v out=%+v", q, err, out)
		}
	}
	t.Cleanup(func() {
		_, _ = myEx.Execute(context.Background(), Request{SQL: "DROP TABLE IF EXISTS mig_src", Approval: true})
	})

	// 目标表：按跨引擎预检 + 人工决策生成 DDL。
	plan, ddlText, err := PlanMigrationBetween(ctx, myEx, pgEx, "mig_src", "mig_src", map[string]string{
		"id": "integer", "v": "varchar(32)",
	})
	if err != nil {
		t.Fatalf("预检: %v（plan=%+v）", err, plan)
	}
	if !strings.Contains(ddlText, "PRIMARY KEY") {
		t.Fatalf("跨引擎 DDL 应带主键：\n%s", ddlText)
	}
	for _, q := range []string{"DROP TABLE IF EXISTS mig_src", ddlText} {
		if out, err := pgEx.Execute(ctx, Request{SQL: q, Approval: true, Actor: "human:test"}); err != nil || out.Err != nil {
			t.Fatalf("建目标表（%s）: err=%v out=%+v", q, err, out)
		}
	}
	t.Cleanup(func() {
		_, _ = pgEx.Execute(context.Background(), Request{SQL: "DROP TABLE IF EXISTS mig_src", Approval: true})
	})

	rep, err := MigrateTable(ctx, myEx, pgEx, "mig_src", "mig_src", MigrateExecOptions{BatchSize: 2})
	if err != nil {
		t.Fatalf("跨引擎搬运: %v", err)
	}
	if rep.Rows != 3 || !rep.Verified {
		t.Fatalf("搬运结果不符：%+v", rep)
	}
	if rep.SourceDigest != rep.TargetDigest {
		t.Fatalf("跨引擎摘要不一致：%s vs %s", rep.SourceDigest, rep.TargetDigest)
	}
}

// openReal 按 DSN 打开一个真实引擎的连接器（user:pass@host:port）。
func openReal(t *testing.T, engine, dsn string) driver.Connector {
	t.Helper()
	at := strings.LastIndex(dsn, "@")
	if at <= 0 {
		t.Fatalf("DSN 格式应为 user:pass@host:port，实际 %q", dsn)
	}
	cred, addr := dsn[:at], dsn[at+1:]
	parts := strings.SplitN(cred, ":", 2)
	if len(parts) != 2 {
		t.Fatalf("凭据部分非法: %q", cred)
	}
	colon := strings.LastIndex(addr, ":")
	if colon <= 0 {
		t.Fatalf("地址部分缺少端口: %q", addr)
	}
	port := 0
	for _, ch := range addr[colon+1:] {
		port = port*10 + int(ch-'0')
	}
	var c driver.Connector
	switch engine {
	case "mysql":
		c = mysql.New()
	case "pg":
		c = pg.New()
	default:
		t.Fatalf("未知引擎 %q", engine)
	}
	if err := c.Open(context.Background(), driver.ConnectConfig{
		Host: addr[:colon], Port: port, User: parts[0], Password: parts[1],
		Catalog: "dbrover_test", AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
	}); err != nil {
		t.Fatalf("打开 %s: %v", engine, err)
	}
	return c
}
