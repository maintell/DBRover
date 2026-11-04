package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
)

// TestListsTriggers 验证 Capabilities.Triggers = true 这句话有实现兜着。
//
// MySQL / PG 也声明过 true，但 Objects(KindTrigger) 根本没有分支——那种
// 声明会让人把「查不到」当成「没有触发器」。SQLite 走 sqlite_master，是真的。
func TestListsTriggers(t *testing.T) {
	ctx := context.Background()
	c := sqlite.New()
	if err := c.Open(ctx, driver.ConnectConfig{
		Host:        filepath.Join(t.TempDir(), "trig.db"),
		AccessMode:  model.AccessReadWrite,
		Environment: model.EnvTest,
	}); err != nil {
		t.Fatalf("打开 sqlite: %v", err)
	}
	defer func() { _ = c.Close() }()

	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE audit (note TEXT)`,
		`CREATE TRIGGER trg_t AFTER INSERT ON t BEGIN INSERT INTO audit (note) VALUES ('x'); END`,
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("准备数据 (%s): %v", q, err)
		}
	}
	_ = s.Close()

	objs, err := c.Meta().Objects(ctx, "", "", model.KindTrigger)
	if err != nil {
		t.Fatalf("Objects: %v", err)
	}
	if len(objs) != 1 || objs[0].Name != "trg_t" {
		t.Fatalf("触发器列表 = %+v，期望只有 trg_t", objs)
	}
	if objs[0].Kind != model.KindTrigger {
		t.Errorf("Kind = %q，期望 trigger", objs[0].Kind)
	}
}
