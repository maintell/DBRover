package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
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
