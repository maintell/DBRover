package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestBackupCompressedRoundTrip 验证压缩容器：头里置位、体积确实变小、
// 恢复出来的数据与未压缩一致。
func TestBackupCompressedRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := openBackupTestDB(t)
	s, err := src.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 造一批重复文本，让压缩效果可观察（几行的库压完反而更大）。
	for i := 0; i < 500; i++ {
		if _, err := s.Execute(ctx,
			"INSERT INTO ra (id, v) VALUES (?, ?)",
			[]any{i + 10, "abcdefghijklmnopqrstuvwxyz-0123456789"}); err != nil {
			t.Fatalf("插入: %v", err)
		}
	}
	_ = s.Close()

	ex := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()
	var plain, packed bytes.Buffer
	if _, err := ex.Backup(ctx, BackupRequest{}, &plain); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := ex.Backup(ctx, BackupRequest{Compress: true}, &packed); err != nil {
		t.Fatalf("Backup(压缩): %v", err)
	}
	if packed.Len() >= plain.Len() {
		t.Fatalf("压缩后 %d 字节，未压缩 %d 字节：压缩没生效", packed.Len(), plain.Len())
	}

	h, err := InspectBackup(bytes.NewReader(packed.Bytes()))
	if err != nil {
		t.Fatalf("InspectBackup: %v", err)
	}
	if h.Flags&backupFlagCompressed == 0 {
		t.Fatalf("头部 flags 没有标记压缩：0x%02x", h.Flags)
	}

	dst := openEmptySQLite(t)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer dstEx.Close()
	rep, err := dstEx.Restore(ctx, bytes.NewReader(packed.Bytes()), RestoreRequest{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(rep.Mismatches) != 0 {
		t.Fatalf("计数不一致：%v", rep.Mismatches)
	}
}

// TestBackupResumeKeepsCompression 验证续跑沿用文件里的 flags，而不是按本次
// 请求重写——同一个文件前后两种格式会让读端没法解析。
func TestBackupResumeKeepsCompression(t *testing.T) {
	ctx := context.Background()
	src := openBackupTestDB(t)
	ex := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	path := filepath.Join(t.TempDir(), "packed.dbr")
	n := 0
	_, err := ex.BackupToFile(ctx, BackupRequest{
		Compress: true,
		afterSection: func(string) error {
			n++
			if n == 1 {
				return errors.New("模拟中断")
			}
			return nil
		},
	}, path)
	if err == nil {
		t.Fatal("应当中断")
	}
	// 续跑时**不带** Compress：头里的 flags 必须仍然是压缩。
	if _, err := ex.BackupToFile(ctx, BackupRequest{Resume: true}, path); err != nil {
		t.Fatalf("续跑: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h, err := InspectBackup(f)
	if err != nil {
		t.Fatalf("InspectBackup: %v", err)
	}
	if h.Flags&backupFlagCompressed == 0 {
		t.Fatalf("续跑把压缩标志弄丢了：0x%02x", h.Flags)
	}
}
