package service_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// openCompareSide 建一个独立的 SQLite 库并执行建表语句。
func openCompareSide(t *testing.T, dir, name string, ddl ...string) driver.Connector {
	t.Helper()
	ctx := context.Background()
	c := sqlite.New()
	if err := c.Open(ctx, driver.ConnectConfig{
		Host: filepath.Join(dir, name), AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
	}); err != nil {
		t.Fatalf("打开 %s: %v", name, err)
	}
	t.Cleanup(func() { _ = c.Close() })

	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	for _, q := range ddl {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("执行 %s: %v", q, err)
		}
	}
	return c
}

// TestCompareSchemas 验证三分类结论与脚本生成：
// 仅右侧的表给出建表语句，右侧新增的列给出加列语句，
// 删除与改类型一律进 need_review，不自动生成。
func TestCompareSchemas(t *testing.T) {
	dir := t.TempDir()
	left := openCompareSide(t, dir, "left.db",
		`CREATE TABLE same (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE t1 (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE gone (id INTEGER PRIMARY KEY)`,
	)
	right := openCompareSide(t, dir, "right.db",
		`CREATE TABLE same (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE t1 (id INTEGER PRIMARY KEY, v VARCHAR(64), extra TEXT)`,
		`CREATE TABLE newtab (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)`,
	)

	res, err := service.Compare(context.Background(), left, right)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if res.LeftEngine != model.EngineSQLite || res.RightEngine != model.EngineSQLite {
		t.Errorf("引擎识别不对: %s / %s", res.LeftEngine, res.RightEngine)
	}

	byName := map[string]service.TableDiff{}
	for _, td := range res.Tables {
		byName[td.Name] = td
	}
	if _, ok := byName["same"]; ok {
		t.Error("相同的表不该出现在差异清单里")
	}
	if byName["gone"].Status != service.CompareOnlyLeft {
		t.Errorf("gone 状态 = %s, 期望 only_left", byName["gone"].Status)
	}
	if byName["newtab"].Status != service.CompareOnlyRight {
		t.Errorf("newtab 状态 = %s, 期望 only_right", byName["newtab"].Status)
	}
	if byName["t1"].Status != service.CompareChanged && byName["t1"].Status != service.CompareNeedReview {
		t.Errorf("t1 状态 = %s, 期望 changed 或 need_review", byName["t1"].Status)
	}

	// 升级脚本：建缺失的表 + 加缺失的列；不该出现 DROP。
	upgrade := strings.Join(res.Upgrade, "\n")
	if !strings.Contains(upgrade, "newtab") {
		t.Errorf("升级脚本缺建表语句: %s", upgrade)
	}
	if !strings.Contains(upgrade, "ADD COLUMN") || !strings.Contains(upgrade, "extra") {
		t.Errorf("升级脚本缺加列语句: %s", upgrade)
	}
	if strings.Contains(strings.ToUpper(upgrade), "DROP ") {
		t.Errorf("升级脚本不该自动生成 DROP: %s", upgrade)
	}

	// 需要人工决策的点必须被列出来（删表 / 删列 / 改类型）。
	review := strings.Join(res.NeedReview, "\n")
	for _, want := range []string{"gone", "extra", "v"} {
		if !strings.Contains(review, want) {
			t.Errorf("need_review 缺 %q: %s", want, review)
		}
	}
}

// TestCompareSameSchemaIsEmpty 两侧完全一致时不该有任何差异项。
func TestCompareSameSchemaIsEmpty(t *testing.T) {
	dir := t.TempDir()
	ddl := []string{
		`CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE b (id INTEGER PRIMARY KEY, n INTEGER NOT NULL DEFAULT 0)`,
	}
	left := openCompareSide(t, dir, "l.db", ddl...)
	right := openCompareSide(t, dir, "r.db", ddl...)

	res, err := service.Compare(context.Background(), left, right)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(res.Tables) != 0 {
		t.Errorf("结构相同不该有差异: %+v", res.Tables)
	}
	if len(res.Upgrade) != 0 || len(res.Downgrade) != 0 {
		t.Errorf("结构相同不该生成脚本: %v / %v", res.Upgrade, res.Downgrade)
	}
}
