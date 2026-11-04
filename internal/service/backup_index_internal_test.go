package service

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
)

// TestBackupKeepsIndexes 锁住一个曾经的真缺陷：导出的 SQL 里没有二级索引，
// 恢复后表是「裸」的——唯一索引丢失意味着唯一性不再被强制，这比慢更严重。
//
// 同时验证约束索引不会被重复创建：UNIQUE 列自动建的索引随建表语句一起重建，
// 再补一条同名 CREATE INDEX 会让整个恢复失败。
func TestBackupKeepsIndexes(t *testing.T) {
	ctx := context.Background()
	open := func(name string) *sqlite.Connector {
		c := sqlite.New()
		if err := c.Open(ctx, driver.ConnectConfig{
			Host:        filepath.Join(t.TempDir(), name),
			AccessMode:  model.AccessReadWrite,
			Environment: model.EnvTest,
		}); err != nil {
			t.Fatalf("打开 %s: %v", name, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}

	src := open("src.db")
	s, err := src.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT, b TEXT UNIQUE, c TEXT)",
		"CREATE INDEX idx_t_a ON t (a)",
		"CREATE UNIQUE INDEX idx_t_c ON t (c)",
		"INSERT INTO t (id, a, b, c) VALUES (1, 'x', 'y', 'z')",
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = s.Close()

	var buf bytes.Buffer
	srcEx := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer srcEx.Close()
	if _, err := srcEx.Backup(ctx, BackupRequest{}, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	script := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("CREATE INDEX")) {
		t.Fatalf("导出脚本里没有 CREATE INDEX：\n%s", script)
	}

	dst := open("dst.db")
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer dstEx.Close()
	rep, err := dstEx.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(rep.Objects) != 1 || rep.Objects[0].Indexes < 2 {
		t.Fatalf("报告里的索引数不对：%+v（至少应有 idx_t_a 与 idx_t_c）", rep.Objects)
	}

	detail, err := dstEx.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: "t"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, ix := range detail.Indexes {
		got[ix.Name] = true
	}
	for _, want := range []string{"idx_t_a", "idx_t_c"} {
		if !got[want] {
			t.Errorf("恢复后缺少索引 %s（实际 %v）", want, detail.Indexes)
		}
	}

	// 唯一性必须仍然被强制：往恢复后的表里插一条重复 c，必须报错。
	ds, err := dst.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ds.Close() }()
	if _, err := ds.Execute(ctx, "INSERT INTO t (id, a, b, c) VALUES (2, 'x2', 'y2', 'z')", nil); err == nil {
		t.Error("恢复后唯一索引没有生效：重复的 c 被插进去了")
	}
}
