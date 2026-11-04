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

// TestMigrateForeignKeyCrossEngine 验证 ADR-0013 第 4 条：父表先迁、子表后迁，
// 外键能建出来并真的生效（插孤儿行会被拒）。
func TestMigrateForeignKeyCrossEngine(t *testing.T) {
	myDSN := os.Getenv("DBRover_TEST_MYSQL")
	pgDSN := os.Getenv("DBRover_TEST_PG")
	if myDSN == "" || pgDSN == "" {
		t.Skip("需要 DBRover_TEST_MYSQL 与 DBRover_TEST_PG")
	}
	ctx := context.Background()
	myEx := NewExecutor(openReal(t, "mysql", myDSN), model.EnvTest, model.AccessReadWrite, nil)
	pgEx := NewExecutor(openReal(t, "pg", pgDSN), model.EnvTest, model.AccessReadWrite, nil)
	defer myEx.Close()
	defer pgEx.Close()

	for _, q := range []string{
		"DROP TABLE IF EXISTS mig_child",
		"DROP TABLE IF EXISTS mig_parent",
		"CREATE TABLE mig_parent (id INT PRIMARY KEY)",
		"CREATE TABLE mig_child (id INT PRIMARY KEY, pid INT, CONSTRAINT fk_child_pid FOREIGN KEY (pid) REFERENCES mig_parent(id) ON DELETE CASCADE)",
		"INSERT INTO mig_parent (id) VALUES (1)",
		"INSERT INTO mig_child (id, pid) VALUES (10, 1)",
	} {
		if out, err := myEx.Execute(ctx, Request{SQL: q, Approval: true}); err != nil || out.Err != nil {
			t.Fatalf("准备 MySQL（%s）: %v %+v", q, err, out)
		}
	}
	t.Cleanup(func() {
		_, _ = myEx.Execute(context.Background(), Request{SQL: "DROP TABLE IF EXISTS mig_child", Approval: true})
		_, _ = myEx.Execute(context.Background(), Request{SQL: "DROP TABLE IF EXISTS mig_parent", Approval: true})
	})
	for _, q := range []string{"DROP TABLE IF EXISTS mig_child", "DROP TABLE IF EXISTS mig_parent"} {
		_, _ = pgEx.Execute(ctx, Request{SQL: q, Approval: true})
	}
	t.Cleanup(func() {
		_, _ = pgEx.Execute(context.Background(), Request{SQL: "DROP TABLE IF EXISTS mig_child", Approval: true})
		_, _ = pgEx.Execute(context.Background(), Request{SQL: "DROP TABLE IF EXISTS mig_parent", Approval: true})
	})

	// 父表先迁。
	if _, ddlText, err := PlanMigrationBetween(ctx, myEx, pgEx, "mig_parent", "", map[string]string{"id": "integer"}); err != nil {
		t.Fatalf("父表预检: %v", err)
	} else if out, e := pgEx.Execute(ctx, Request{SQL: ddlText, Approval: true}); e != nil || out.Err != nil {
		t.Fatalf("建父表: %v %+v", e, out)
	}
	if _, err := MigrateTable(ctx, myEx, pgEx, "mig_parent", "", MigrateExecOptions{}); err != nil {
		t.Fatalf("搬父表: %v", err)
	}

	// 子表后迁（含外键定义）。
	if _, ddlText, err := PlanMigrationBetween(ctx, myEx, pgEx, "mig_child", "", map[string]string{
		"id": "integer", "pid": "integer",
	}); err != nil {
		t.Fatalf("子表预检: %v", err)
	} else if out, e := pgEx.Execute(ctx, Request{SQL: ddlText, Approval: true}); e != nil || out.Err != nil {
		t.Fatalf("建子表: %v %+v", e, out)
	}
	if _, err := MigrateTable(ctx, myEx, pgEx, "mig_child", "", MigrateExecOptions{}); err != nil {
		t.Fatalf("搬子表: %v", err)
	}

	// 搬完后补建外键：此时父表已在目标库，应当建成。
	detail, err := myEx.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: "mig_child"})
	if err != nil {
		t.Fatalf("读源子表结构: %v", err)
	}
	stmts, skipped := BuildMigrationForeignKeyDDL(model.EnginePostgres, true, "mig_child", detail,
		func(n string) bool { return strings.EqualFold(n, "mig_parent") })
	if len(skipped) != 0 {
		t.Fatalf("父表已在目标库，不该跳过：%v", skipped)
	}
	if len(stmts) != 1 {
		t.Fatalf("应当生成 1 条外键语句：%v", stmts)
	}
	if out, e := pgEx.Execute(ctx, Request{SQL: stmts[0], Approval: true}); e != nil || out.Err != nil {
		t.Fatalf("建外键: %v %+v", e, out)
	}

	// 外键真的生效：插孤儿行必须失败。
	if out, e := pgEx.Execute(ctx, Request{SQL: "INSERT INTO mig_child (id, pid) VALUES (99, 12345)", Approval: true}); e == nil && (out == nil || out.Err == nil) {
		t.Fatal("插孤儿行应当被外键拒绝")
	}
}

// TestMigrateCommentsOnRealPG 验证注释语句在真库上能执行、能读回。
//
// 单测只能证明我们拼出的 SQL 形状对；能不能被引擎接受、中文能不能原样存下来，
// 只有真库说了算。
func TestMigrateCommentsOnRealPG(t *testing.T) {
	dsn := os.Getenv("DBRover_TEST_PG")
	if dsn == "" {
		t.Skip("需要 DBRover_TEST_PG")
	}
	ctx := context.Background()
	pgEx := NewExecutor(openReal(t, "pg", dsn), model.EnvTest, model.AccessReadWrite, nil)
	defer pgEx.Close()

	for _, q := range []string{"DROP TABLE IF EXISTS mig_comment", "CREATE TABLE mig_comment (a int)"} {
		if out, err := pgEx.Execute(ctx, Request{SQL: q, Approval: true}); err != nil || out.Err != nil {
			t.Fatalf("准备 PG（%s）: %v %+v", q, err, out)
		}
	}
	t.Cleanup(func() {
		_, _ = pgEx.Execute(context.Background(), Request{SQL: "DROP TABLE IF EXISTS mig_comment", Approval: true})
	})

	src := &model.TableDetail{
		Comment: "表的说明",
		Columns: []model.Column{{Name: "a", Comment: "列'说明"}},
	}
	stmts, skipped := BuildMigrationCommentDDL(model.EnginePostgres, "mig_comment", src)
	if len(skipped) != 0 {
		t.Fatalf("PG 不该跳过：%v", skipped)
	}
	for _, stmt := range stmts {
		if out, err := pgEx.Execute(ctx, Request{SQL: stmt, Approval: true}); err != nil || out.Err != nil {
			t.Fatalf("执行注释（%s）: %v %+v", stmt, err, out)
		}
	}

	sess, err := pgEx.conn.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	cur, err := sess.Query(ctx, "SELECT obj_description('mig_comment'::regclass,'pg_class'), col_description('mig_comment'::regclass, 1)", nil, driver.QueryOptions{MaxRows: 1})
	if err != nil {
		t.Fatalf("读注释: %v", err)
	}
	defer func() { _ = cur.Close() }()
	rows, _, err := cur.Next(ctx)
	if err != nil || len(rows) != 1 || len(rows[0]) != 2 {
		t.Fatalf("读注释结果异常: %v %v", err, rows)
	}
	if got, _ := rows[0][0].(string); got != "表的说明" {
		t.Errorf("表注释 = %q", got)
	}
	if got, _ := rows[0][1].(string); got != "列'说明" {
		t.Errorf("列注释 = %q（单引号应原样保留）", got)
	}
}

// TestMigrateCommentsOnRealMySQL 验证 MySQL 只有表注释能搬、且反斜杠不被改写。
//
// MySQL 默认 sql_mode 不含 NO_BACKSLASH_ESCAPES，反斜杠在字符串里是转义符：
// 只双写单引号的话，注释里的路径会被存成带制表符的另一段文本——语句不报错、
// 用户却看不到自己写的注释。这条用例就是为了让那种「看起来对」的拼接立刻变红。
func TestMigrateCommentsOnRealMySQL(t *testing.T) {
	dsn := os.Getenv("DBRover_TEST_MYSQL")
	if dsn == "" {
		t.Skip("需要 DBRover_TEST_MYSQL")
	}
	ctx := context.Background()
	myEx := NewExecutor(openReal(t, "mysql", dsn), model.EnvTest, model.AccessReadWrite, nil)
	defer myEx.Close()

	for _, q := range []string{"DROP TABLE IF EXISTS mig_comment", "CREATE TABLE mig_comment (a INT)"} {
		if out, err := myEx.Execute(ctx, Request{SQL: q, Approval: true}); err != nil || out.Err != nil {
			t.Fatalf("准备 MySQL（%s）: %v %+v", q, err, out)
		}
	}
	t.Cleanup(func() {
		_, _ = myEx.Execute(context.Background(), Request{SQL: "DROP TABLE IF EXISTS mig_comment", Approval: true})
	})

	const bs = "\\" // 反斜杠本身
	want := "表说明 C:" + bs + "temp 与 '引号'"
	src := &model.TableDetail{
		Comment: want,
		Columns: []model.Column{{Name: "a", Comment: "列说明"}},
	}
	stmts, skipped := BuildMigrationCommentDDL(model.EngineMySQL, "mig_comment", src)
	if len(stmts) != 1 {
		t.Fatalf("MySQL 只该生成表注释：%v", stmts)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "列注释未迁移") {
		t.Fatalf("列注释要说明跳过原因：%v", skipped)
	}
	if out, err := myEx.Execute(ctx, Request{SQL: stmts[0], Approval: true}); err != nil || out.Err != nil {
		t.Fatalf("执行注释（%s）: %v %+v", stmts[0], err, out)
	}

	// 读回真值：反斜杠与单引号都必须原样在。
	sess, err := myEx.conn.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	cur, err := sess.Query(ctx, "SELECT TABLE_COMMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mig_comment'", nil, driver.QueryOptions{MaxRows: 1})
	if err != nil {
		t.Fatalf("读注释: %v", err)
	}
	defer func() { _ = cur.Close() }()
	rows, _, err := cur.Next(ctx)
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("读注释结果异常: %v %v", err, rows)
	}
	if got, _ := rows[0][0].(string); got != want {
		t.Errorf("表注释 = %q，期望 %q", got, want)
	}
}

// TestProbeLengthsCountsCharacters 验证探测的是**字符数**而不是字节数。
//
// 这条必须真机跑：SQLite 只有 LENGTH、MySQL/PG 是 CHAR_LENGTH，而 MySQL 的
// LENGTH 返回字节数——用错一个，「中文abc」就从 5 变成 8，判定「放得下」的
// 阈值随之整体漂移。
func TestProbeLengthsCountsCharacters(t *testing.T) {
	ctx := context.Background()
	const want = 5 // "中文abc"

	check := func(t *testing.T, ex *Executor) {
		t.Helper()
		for _, q := range []string{"DROP TABLE IF EXISTS probe_len", "CREATE TABLE probe_len (id INT PRIMARY KEY, v VARCHAR(64))"} {
			if out, err := ex.Execute(ctx, Request{SQL: q, Approval: true}); err != nil || out.Err != nil {
				t.Fatalf("准备（%s）: %v %+v", q, err, out)
			}
		}
		t.Cleanup(func() {
			_, _ = ex.Execute(context.Background(), Request{SQL: "DROP TABLE IF EXISTS probe_len", Approval: true})
		})
		if out, err := ex.Execute(ctx, Request{SQL: `INSERT INTO probe_len (id,v) VALUES (1,'中文abc'),(2,'x')`, Approval: true}); err != nil || out.Err != nil {
			t.Fatalf("插数据: %v %+v", err, out)
		}
		got, err := ProbeLengths(ctx, ex, "probe_len", []string{"v"})
		if err != nil {
			t.Fatalf("ProbeLengths: %v", err)
		}
		if got["v"] != want {
			t.Fatalf("实际最大长度 = %d，期望 %d（字符数；按字节算会是 8）", got["v"], want)
		}
	}

	t.Run("sqlite", func(t *testing.T) {
		ex := NewExecutor(openMigrateDB(t, "probe.db"), model.EnvTest, model.AccessReadWrite, nil)
		defer ex.Close()
		check(t, ex)
	})
	if dsn := os.Getenv("DBRover_TEST_MYSQL"); dsn != "" {
		t.Run("mysql", func(t *testing.T) {
			ex := NewExecutor(openReal(t, "mysql", dsn), model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()
			check(t, ex)
		})
	}
	if dsn := os.Getenv("DBRover_TEST_PG"); dsn != "" {
		t.Run("pg", func(t *testing.T) {
			ex := NewExecutor(openReal(t, "pg", dsn), model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()
			check(t, ex)
		})
	}
}
