package service

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestBackupEncryptedRoundTrip 覆盖 docs/08 验收第 5 条：加密容器能用正确口令
// 打开；口令不对必须明确失败，而不是解出一堆乱码。
func TestBackupEncryptedRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := openBackupTestDB(t)
	ex := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	const pass = "correct horse battery staple"
	var buf bytes.Buffer
	if _, err := ex.Backup(ctx, BackupRequest{Passphrase: pass, Compress: true}, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// 头是明文的（要能列出对象），但不能带任何数据。
	h, err := InspectBackup(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("InspectBackup: %v", err)
	}
	if h.Flags&backupFlagEncrypted == 0 {
		t.Fatalf("flags 没有标记加密：0x%02x", h.Flags)
	}
	if h.KDF != backupKDF || h.Salt == "" || h.Iterations == 0 {
		t.Fatalf("加密参数没写进头：%+v", h)
	}
	if bytes.Contains(buf.Bytes(), []byte("CREATE TABLE")) {
		t.Fatal("加密后仍能在文件里看到明文 SQL")
	}

	dst := openEmptySQLite(t)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer dstEx.Close()
	rep, err := dstEx.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{Passphrase: pass})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rep.Rows != 3 {
		t.Errorf("恢复行数 = %d，期望 3", rep.Rows)
	}
}

// TestRestoreNeedsPassphrase 保证没有口令时给出可操作的错误。
func TestRestoreNeedsPassphrase(t *testing.T) {
	ctx := context.Background()
	src := openBackupTestDB(t)
	ex := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()
	var buf bytes.Buffer
	if _, err := ex.Backup(ctx, BackupRequest{Passphrase: "pw"}, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	dst := openEmptySQLite(t)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer dstEx.Close()
	_, err := dstEx.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{})
	if !errors.Is(err, ErrBackupNeedPassphrase) {
		t.Fatalf("缺少口令应返回 ErrBackupNeedPassphrase，实际 %v", err)
	}
}

// TestRestoreWrongPassphrase 保证口令不对时说清「可能是口令错，也可能是文件被改」。
func TestRestoreWrongPassphrase(t *testing.T) {
	ctx := context.Background()
	src := openBackupTestDB(t)
	ex := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()
	var buf bytes.Buffer
	if _, err := ex.Backup(ctx, BackupRequest{Passphrase: "right"}, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	dst := openEmptySQLite(t)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer dstEx.Close()
	_, err := dstEx.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{Passphrase: "wrong"})
	if !errors.Is(err, ErrBackupBadPassphrase) {
		t.Fatalf("口令不对应返回 ErrBackupBadPassphrase，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "改动") {
		t.Errorf("错误里要提到「也可能被改动」，实际 %q", err.Error())
	}
}
