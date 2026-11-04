package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
)

func openEmptySQLite(t *testing.T) *sqlite.Connector {
	t.Helper()
	c := sqlite.New()
	if err := c.Open(context.Background(), driver.ConnectConfig{
		Host:        filepath.Join(t.TempDir(), "dst.db"),
		AccessMode:  model.AccessReadWrite,
		Environment: model.EnvTest,
	}); err != nil {
		t.Fatalf("打开目标库: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestRestoreRoundTrip 覆盖 docs/08 验收第 3、4 条的主体：
// 备份 → 恢复到空库 → 对象数与行数都对得上。
func TestRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := openBackupTestDB(t)
	srcEx := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer srcEx.Close()

	var buf bytes.Buffer
	if _, err := srcEx.Backup(ctx, BackupRequest{}, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	dst := openEmptySQLite(t)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer dstEx.Close()

	rep, err := dstEx.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(rep.Objects) != 3 {
		t.Fatalf("恢复 %d 个对象，期望 3", len(rep.Objects))
	}
	if rep.Rows != 3 {
		t.Fatalf("恢复 %d 行，期望 3（每表 1 行）", rep.Rows)
	}
	for _, o := range rep.Objects {
		if o.Rows != 1 {
			t.Errorf("%s 恢复 %d 行，期望 1", o.Name, o.Rows)
		}
	}

	// 目标库里要真的能查到数据，而不只是报告里的数字。
	objs, err := dst.Meta().Objects(ctx, "", "", model.KindTable)
	if err != nil {
		t.Fatalf("列出目标对象: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("目标库有 %d 张表，期望 3", len(objs))
	}
}

// TestRestoreRefusesNonEmptyTarget 对应 docs/08 §3.1 的硬性约束：
// 目标已有同名对象时必须拒绝，并点名冲突对象。
func TestRestoreRefusesNonEmptyTarget(t *testing.T) {
	ctx := context.Background()
	src := openBackupTestDB(t)
	ex := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	var buf bytes.Buffer
	if _, err := ex.Backup(ctx, BackupRequest{}, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	_, err := ex.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{})
	if !errors.Is(err, ErrRestoreTarget) {
		t.Fatalf("目标非空应返回 ErrRestoreTarget，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "已存在") && !strings.Contains(err.Error(), "已有同名对象") {
		t.Errorf("错误里要点名冲突，实际 %q", err.Error())
	}
}

// TestRestoreRejectsOtherEngine 保证跨引擎恢复被明确拒绝，而不是拼错语句半途失败。
func TestRestoreRejectsOtherEngine(t *testing.T) {
	hdr := BackupHeader{
		FormatVersion: backupFormatVersion,
		Engine:        "mysql",
		Objects:       []BackupObject{{Kind: "table", Name: "x"}},
	}
	hj, err := json.Marshal(hdr)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.WriteString(backupMagic)
	if err := binary.Write(&buf, binary.LittleEndian, uint16(backupFormatVersion)); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint16(0)); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint32(len(hj))); err != nil {
		t.Fatal(err)
	}
	buf.Write(hj)
	sec := []byte("CREATE TABLE x (id INTEGER PRIMARY KEY)")
	if err := binary.Write(&buf, binary.LittleEndian, uint32(len(sec))); err != nil {
		t.Fatal(err)
	}
	buf.Write(sec)

	dst := openEmptySQLite(t)
	ex := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()
	_, err = ex.Restore(context.Background(), bytes.NewReader(buf.Bytes()), RestoreRequest{})
	if !errors.Is(err, ErrRestoreTarget) || !strings.Contains(err.Error(), "同引擎") {
		t.Fatalf("跨引擎应明确拒绝并提到同引擎，实际 %v", err)
	}
}
