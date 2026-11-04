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

// noRollbackConn 把 SQLite 连接伪装成「DDL 不能回滚」的引擎（例如 MySQL），
// 专门用来跑覆盖模式的那条拒绝分支——它在碰数据之前就返回了，所以不必真的起
// 一个 MySQL。
type noRollbackConn struct{ driver.Connector }

func (c noRollbackConn) Capabilities() model.Capabilities {
	caps := c.Connector.Capabilities()
	caps.DDLTransactional = false
	return caps
}

// TestRestoreRequiresMatchingTargetName 对应 docs/08 验收 3 里那句「输入目标库名」：
// 目标非空时必须把目标名写出来，且写错要当场拦住。
//
// 这一条同时补上了一个**此前不成立**的能力：`-allow-existing` 只是放行，
// 但恢复执行的是容器里的 `CREATE TABLE`（不带 IF NOT EXISTS），目标表已存在时
// 必然失败。现在覆盖是「确认 → 先删冲突对象 → 再建」，并在报告里点名被替换的。
func TestRestoreRequiresMatchingTargetName(t *testing.T) {
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
	if _, err := dstEx.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{}); err != nil {
		t.Fatalf("首次恢复到空库: %v", err)
	}

	// 1) 目标已非空、没写名字：拒绝，并说清「怎么写」。
	_, err := dstEx.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{})
	if !errors.Is(err, ErrRestoreTarget) {
		t.Fatalf("目标非空应返回 ErrRestoreTarget，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "写出来") {
		t.Errorf("要说清怎么确认，实际 %q", err.Error())
	}

	// 2) 名字写错：拒绝，并把「你写的」与「实际指向」都摆出来。
	_, err = dstEx.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{ConfirmTarget: "别的库.db"})
	if !errors.Is(err, ErrRestoreTarget) {
		t.Fatalf("写错目标名应返回 ErrRestoreTarget，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "别的库.db") || !strings.Contains(err.Error(), dst.Target()) {
		t.Errorf("错误里要同时出现「你写的」与「实际的」目标：%q", err.Error())
	}

	// 3) 名字写对：覆盖成功，报告点名被替换的对象，行数不叠加。
	rep, err := dstEx.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{ConfirmTarget: dst.Target()})
	if err != nil {
		t.Fatalf("写对目标名应当放行: %v", err)
	}
	if len(rep.Replaced) != 3 {
		t.Errorf("报告应点名 3 个被替换对象，实际 %v", rep.Replaced)
	}
	for _, o := range rep.Objects {
		if o.Rows != 1 {
			t.Errorf("%s 覆盖后 %d 行，期望 1（覆盖不是叠加）", o.Name, o.Rows)
		}
	}
}

// TestRestoreRefusesOverwriteWhenDDLCannotRollBack：覆盖要先删对象，而删除不可逆。
//
// 只在 DDL 能整体回滚的引擎上做；MySQL 的 DDL 隐式提交，中途失败会留下
// 「旧的删了、新的没建起来」的库——那种事不该由一个开关授权。
func TestRestoreRefusesOverwriteWhenDDLCannotRollBack(t *testing.T) {
	ctx := context.Background()
	src := openBackupTestDB(t)
	srcEx := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer srcEx.Close()

	var buf bytes.Buffer
	if _, err := srcEx.Backup(ctx, BackupRequest{}, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	dst := openEmptySQLite(t)
	first := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer first.Close()
	if _, err := first.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{}); err != nil {
		t.Fatalf("首次恢复到空库: %v", err)
	}

	ex := NewExecutor(noRollbackConn{Connector: dst}, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()
	_, err := ex.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{ConfirmTarget: dst.Target()})
	if !errors.Is(err, ErrRestoreTarget) {
		t.Fatalf("不可回滚的引擎不该允许覆盖，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不能回滚") {
		t.Errorf("要说清拦住的原因，实际 %q", err.Error())
	}
}
