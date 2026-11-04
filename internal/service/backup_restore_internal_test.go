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

// TestRestoreRefusesSource 覆盖 docs/08 §3.1 第 2 条：禁止恢复到源库。
//
// 身份串由调用方给出，所以这条只能防手滑；测试同时确认「不同身份可以恢复」，
// 否则这道理查会变成谁也恢复不了。
func TestRestoreRefusesSource(t *testing.T) {
	ctx := context.Background()
	src := openBackupTestDB(t)
	srcEx := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer srcEx.Close()

	var buf bytes.Buffer
	if _, err := srcEx.Backup(ctx, BackupRequest{SourceID: "sqlite:/data/src.db"}, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	h, err := InspectBackup(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("InspectBackup: %v", err)
	}
	if h.SourceID != "sqlite:/data/src.db" {
		t.Fatalf("头部没记住源库身份：%q", h.SourceID)
	}

	dst := openEmptySQLite(t)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer dstEx.Close()
	_, err = dstEx.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{SourceID: "sqlite:/data/src.db"})
	if !errors.Is(err, ErrRestoreTarget) || !strings.Contains(err.Error(), "源库") {
		t.Fatalf("恢复到源库应被拒并说明，实际 %v", err)
	}

	// 换一个身份就能恢复。
	if _, err := dstEx.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{SourceID: "sqlite:/data/dst.db"}); err != nil {
		t.Fatalf("目标不是源库时应当允许：%v", err)
	}
}

// TestRestoreReportsCountMismatch 保证「恢复后与备份时对不上」会被报出来，
// 而不是安静地给一份看起来正常的报告。
//
// 容器里写的是「该表有 99 个索引」，实际脚本只建了主键——数量必然对不上。
func TestRestoreReportsCountMismatch(t *testing.T) {
	hdr := BackupHeader{
		FormatVersion: backupFormatVersion,
		Engine:        "sqlite",
		Objects:       []BackupObject{{Kind: "table", Name: "m", Indexes: 99}},
	}
	hj, err := json.Marshal(hdr)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.WriteString(backupMagic)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(backupFormatVersion))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(hj)))
	buf.Write(hj)
	sec := []byte("CREATE TABLE m (id INTEGER PRIMARY KEY)")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(sec)))
	buf.Write(sec)

	dst := openEmptySQLite(t)
	ex := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()
	rep, err := ex.Restore(context.Background(), bytes.NewReader(buf.Bytes()), RestoreRequest{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(rep.Mismatches) == 0 {
		t.Fatal("索引数不一致时必须报出来")
	}
	if !strings.Contains(rep.Mismatches[0], "索引") {
		t.Errorf("不一致描述里要指明是哪一项，实际 %q", rep.Mismatches[0])
	}
}
