package service_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestBackupInspect 覆盖 docs/08 验收第 1 条：容器能被独立工具打开并列出对象。
//
// 这里刻意用**测试自己实现的解析**去读容器，而不是复用产器的解析函数——
// 复用会让「写错了但读得懂」也算通过。
func TestBackupInspect(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	for _, q := range []string{
		"DROP TABLE IF EXISTS bk_b",
		"DROP TABLE IF EXISTS bk_a",
		"CREATE TABLE bk_a (id INTEGER PRIMARY KEY, v TEXT)",
		"CREATE TABLE bk_b (id INTEGER PRIMARY KEY, n INTEGER)",
		"INSERT INTO bk_a (id, v) VALUES (1, 'x'), (2, 'y')",
		"INSERT INTO bk_b (id, n) VALUES (1, 10)",
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("准备数据 (%s): %v", q, err)
		}
	}

	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	var buf bytes.Buffer
	hdr, err := ex.Backup(ctx, service.BackupRequest{}, &buf)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(hdr.Objects) < 2 {
		t.Fatalf("头部对象数 = %d，期望至少 2（bk_a、bk_b）", len(hdr.Objects))
	}

	// 独立工具视角：重新打开文件头。
	inspected, err := service.InspectBackup(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("InspectBackup: %v", err)
	}
	if inspected.Engine != "sqlite" {
		t.Errorf("engine = %q", inspected.Engine)
	}
	names := map[string]bool{}
	for _, o := range inspected.Objects {
		names[o.Name] = true
	}
	if !names["bk_a"] || !names["bk_b"] {
		t.Fatalf("清单里缺少对象：%+v", inspected.Objects)
	}

	// 按测试自己的解析取出第一段，验证它确实是一段可回放的脚本。
	first, err := readSection(buf.Bytes(), 0)
	if err != nil {
		t.Fatalf("读第一段: %v", err)
	}
	if !strings.Contains(first, "CREATE TABLE") {
		t.Fatalf("第一段不像建表脚本：%q", first[:min(len(first), 80)])
	}
}

// TestInspectBackupRejectsBadInput 保证「不是备份文件」时说人话，而不是 panic。
func TestInspectBackupRejectsBadInput(t *testing.T) {
	if _, err := service.InspectBackup(strings.NewReader("not a backup")); !errors.Is(err, service.ErrBackupFormat) {
		t.Fatalf("非 DBR 文件应返回 ErrBackupFormat，实际 %v", err)
	}
	// 版本号不对：明确说版本，不要含糊。
	bad := []byte("DBROVER\x00")
	bad = append(bad, 0x09, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00)
	bad = append(bad, []byte("{}")...)
	_, err := service.InspectBackup(bytes.NewReader(bad))
	if !errors.Is(err, service.ErrBackupFormat) || !strings.Contains(err.Error(), "版本") {
		t.Fatalf("版本不匹配应指明版本，实际 %v", err)
	}
}

// readSection 是测试侧的独立解析：跳过固定头与 JSON，读第 i 段。
func readSection(data []byte, i int) (string, error) {
	if len(data) < 16 {
		return "", errors.New("太短")
	}
	off := 16
	hlen := binary.LittleEndian.Uint32(data[12:16])
	off += int(hlen)
	for idx := 0; idx <= i; idx++ {
		if off+4 > len(data) {
			return "", errors.New("没有更多段")
		}
		n := int(binary.LittleEndian.Uint32(data[off : off+4]))
		off += 4
		if off+n > len(data) {
			return "", errors.New("段长度越界")
		}
		if idx == i {
			return string(data[off : off+n]), nil
		}
		off += n
	}
	return "", errors.New("没有更多段")
}
