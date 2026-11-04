package service

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
)

// openBackupTestDB 建一个带三张表的 SQLite 库，供续跑用例使用。
func openBackupTestDB(t *testing.T) *sqlite.Connector {
	t.Helper()
	ctx := context.Background()
	c := sqlite.New()
	if err := c.Open(ctx, driver.ConnectConfig{
		Host:        filepath.Join(t.TempDir(), "bk.db"),
		AccessMode:  model.AccessReadWrite,
		Environment: model.EnvTest,
	}); err != nil {
		t.Fatalf("打开 sqlite: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	for _, q := range []string{
		"CREATE TABLE ra (id INTEGER PRIMARY KEY, v TEXT)",
		"INSERT INTO ra (id, v) VALUES (1, 'a')",
		"CREATE TABLE rb (id INTEGER PRIMARY KEY, v TEXT)",
		"INSERT INTO rb (id, v) VALUES (1, 'b')",
		"CREATE TABLE rc (id INTEGER PRIMARY KEY, v TEXT)",
		"INSERT INTO rc (id, v) VALUES (1, 'c')",
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("准备数据 (%s): %v", q, err)
		}
	}
	return c
}

// TestBackupResumeAfterFailure 对应 ADR-0009 的验证计划：
// 模拟「写到一半被 kill」→ 续跑 → 最终容器完整且同一张表不出现两次。
func TestBackupResumeAfterFailure(t *testing.T) {
	ctx := context.Background()
	c := openBackupTestDB(t)
	ex := NewExecutor(c, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	path := filepath.Join(t.TempDir(), "resume.dbr")
	fail := errors.New("模拟中途被杀")
	n := 0
	_, err := ex.BackupToFile(ctx, BackupRequest{
		afterSection: func(string) error {
			n++
			if n == 1 {
				return fail
			}
			return nil
		},
	}, path)
	if !errors.Is(err, fail) {
		t.Fatalf("应当在中途失败，实际 %v", err)
	}
	if _, statErr := os.Stat(path + ".progress"); statErr != nil {
		t.Fatalf("失败后应当留下检查点：%v", statErr)
	}

	// 续跑：成功的路径要把剩下的表补完。
	h, err := ex.BackupToFile(ctx, BackupRequest{Resume: true}, path)
	if err != nil {
		t.Fatalf("续跑失败: %v", err)
	}
	if len(h.Objects) != 3 {
		t.Fatalf("续跑后对象数 = %d，期望 3", len(h.Objects))
	}
	if _, statErr := os.Stat(path + ".progress"); !os.IsNotExist(statErr) {
		t.Errorf("完成后检查点文件应当被删掉，实际 %v", statErr)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读容器: %v", err)
	}
	secs, err := backupSections(data)
	if err != nil {
		t.Fatalf("解析容器: %v", err)
	}
	if len(secs) != 3 {
		t.Fatalf("容器里有 %d 段，期望 3（续跑不能重复写表）", len(secs))
	}
	seen := map[string]int{}
	for _, s := range secs {
		for _, name := range []string{"ra", "rb", "rc"} {
			if strings.Contains(s, "CREATE TABLE "+name+" ") {
				seen[name]++
			}
		}
	}
	for _, name := range []string{"ra", "rb", "rc"} {
		if seen[name] != 1 {
			t.Errorf("表 %s 出现 %d 次，期望 1 次", name, seen[name])
		}
	}
}

// TestBackupResumeNeedsCheckpoint 保证「没有检查点」时明确报错，不静默从头。
func TestBackupResumeNeedsCheckpoint(t *testing.T) {
	ctx := context.Background()
	c := openBackupTestDB(t)
	ex := NewExecutor(c, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	path := filepath.Join(t.TempDir(), "none.dbr")
	_, err := ex.BackupToFile(ctx, BackupRequest{Resume: true}, path)
	if !errors.Is(err, ErrBackupNoCheckpoint) {
		t.Fatalf("缺少检查点时应返回 ErrBackupNoCheckpoint，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "整库重跑") {
		t.Errorf("错误里要说明需整库重跑，实际 %q", err.Error())
	}
}

// backupSections 是测试侧的独立解析：跳过固定头与 header JSON，返回每段文本。
func backupSections(data []byte) ([]string, error) {
	if len(data) < 16 {
		return nil, errors.New("容器太短")
	}
	hlen := int(binary.LittleEndian.Uint32(data[12:16]))
	off := 16 + hlen
	var out []string
	for off+4 <= len(data) {
		n := int(binary.LittleEndian.Uint32(data[off : off+4]))
		off += 4
		if off+n > len(data) {
			return nil, errors.New("段长度越界")
		}
		out = append(out, string(data[off:off+n]))
		off += n
	}
	return out, nil
}
