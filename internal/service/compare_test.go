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

	// 有差异的表要带两侧原文：差异清单说的是「哪一列不对」，
	// 判断该怎么改还得看整张表长什么样。
	if byName["gone"].LeftDDL == "" {
		t.Error("仅左侧的表应带左侧原文")
	}
	if byName["newtab"].RightDDL == "" {
		t.Error("仅右侧的表应带右侧原文")
	}
	if byName["t1"].LeftDDL == "" || byName["t1"].RightDDL == "" {
		t.Error("两侧都有差异的表应带双侧原文")
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

// TestCompareIndexes 对应 docs/07 §4.1 / §4.2 与验收 9：
// 索引要参与比对，且按「列序列 + 唯一性 + 谓词」为键、不按名字；
// 索引差异要同时给出升级与回退脚本（删索引不丢数据，可逆）。
func TestCompareIndexes(t *testing.T) {
	dir := t.TempDir()
	left := openCompareSide(t, dir, "il.db",
		`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE INDEX ix_v ON t(v)`,
	)
	right := openCompareSide(t, dir, "ir.db",
		`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE UNIQUE INDEX ix_v2 ON t(v)`,
		`CREATE INDEX ix_id ON t(id)`,
	)

	res, err := service.Compare(context.Background(), left, right)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(res.Tables) != 1 {
		t.Fatalf("差异表数 = %d, 期望 1: %+v", len(res.Tables), res.Tables)
	}
	td := res.Tables[0]
	if td.Status != service.CompareChanged {
		t.Errorf("表状态 = %s, 期望 changed", td.Status)
	}
	if len(td.Columns) != 0 {
		t.Errorf("列没有差异, 却报了 %d 条: %+v", len(td.Columns), td.Columns)
	}
	if len(td.Indexes) != 3 {
		t.Fatalf("索引差异数 = %d, 期望 3（左侧多 1、右侧多 2）: %+v", len(td.Indexes), td.Indexes)
	}

	upgrade := strings.Join(res.Upgrade, "\n")
	downgrade := strings.Join(res.Downgrade, "\n")
	for _, want := range []string{"CREATE UNIQUE INDEX", "ix_v2", "ix_id", "DROP INDEX"} {
		if !strings.Contains(upgrade, want) {
			t.Errorf("升级脚本缺 %q:\n%s", want, upgrade)
		}
	}
	// 回退脚本必须能把左侧还原：重建被删的 ix_v、删掉新增的两个。
	for _, want := range []string{"CREATE INDEX", "ix_v", "DROP INDEX", "ix_v2", "ix_id"} {
		if !strings.Contains(downgrade, want) {
			t.Errorf("回退脚本缺 %q:\n%s", want, downgrade)
		}
	}
	// 主键索引不参与：不该出现对 PRIMARY / autoindex 的 DROP INDEX。
	if strings.Contains(strings.ToUpper(upgrade), "PRIMARY") {
		t.Errorf("主键索引不该被当成普通索引处理:\n%s", upgrade)
	}
}

// TestCompareIndexNameOnlyDifference 验证「名字不同」不被当成结构差异去生成脚本。
func TestCompareIndexNameOnlyDifference(t *testing.T) {
	dir := t.TempDir()
	left := openCompareSide(t, dir, "nl.db",
		`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE INDEX a_idx ON t(v)`,
	)
	right := openCompareSide(t, dir, "nr.db",
		`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE INDEX b_idx ON t(v)`,
	)

	res, err := service.Compare(context.Background(), left, right)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(res.Upgrade) != 0 || len(res.Downgrade) != 0 {
		t.Errorf("只是改了索引名，不该生成脚本: %v / %v", res.Upgrade, res.Downgrade)
	}
	review := strings.Join(res.NeedReview, "\n")
	if !strings.Contains(review, "索引名不同") {
		t.Errorf("应说明只是索引名不同: %s", review)
	}
	if len(res.Tables) != 1 || len(res.Tables[0].Indexes) != 1 {
		t.Fatalf("应报 1 条索引差异: %+v", res.Tables)
	}
	if res.Tables[0].Indexes[0].Status != service.CompareChanged {
		t.Errorf("状态 = %s, 期望 changed", res.Tables[0].Indexes[0].Status)
	}
}

// TestCompareForeignKeysEndToEnd 走一遍真实结构：右侧多一个外键。
// SQLite 不支持 ALTER TABLE 加约束，所以结论必须是 need_review（说明要重建表），
// 而不是给一条必然失败的语句。
func TestCompareForeignKeysEndToEnd(t *testing.T) {
	dir := t.TempDir()
	left := openCompareSide(t, dir, "fkl.db",
		`CREATE TABLE parent (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER)`,
	)
	right := openCompareSide(t, dir, "fkr.db",
		`CREATE TABLE parent (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id))`,
	)

	res, err := service.Compare(context.Background(), left, right)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var child *service.TableDiff
	for i := range res.Tables {
		if res.Tables[i].Name == "child" {
			child = &res.Tables[i]
		}
	}
	if child == nil {
		t.Fatalf("child 应有差异: %+v", res.Tables)
	}
	if len(child.ForeignKeys) != 1 || child.ForeignKeys[0].Status != service.CompareOnlyRight {
		t.Fatalf("应报一条「仅右侧」的外键: %+v", child.ForeignKeys)
	}

	upgrade := strings.Join(res.Upgrade, "\n")
	if strings.Contains(upgrade, "FOREIGN KEY") {
		t.Errorf("SQLite 不支持加外键，不该生成该语句:\n%s", upgrade)
	}
	review := strings.Join(res.NeedReview, "\n")
	if !strings.Contains(review, "不支持用 ALTER TABLE 加约束") {
		t.Errorf("应说明左侧引擎不支持: %s", review)
	}
}

// TestCompareViews 对应 docs/07 §4.1 / 验收 9：视图定义参与结构比对。
//
// 三个结论都要能出现：仅左侧、仅右侧、查询体不同；相同的视图不出现在清单里。
// 与列的取舍不同：删视图不丢数据（定义就在回退脚本里），所以两个方向都生成脚本。
func TestCompareViews(t *testing.T) {
	dir := t.TempDir()
	left := openCompareSide(t, dir, "viewl.db",
		`CREATE TABLE t (id INTEGER PRIMARY KEY)`,
		`CREATE VIEW v_same AS SELECT id FROM t`,
		`CREATE VIEW v_gone AS SELECT id FROM t`,
		`CREATE VIEW v_chg AS SELECT id FROM t`,
	)
	right := openCompareSide(t, dir, "viewr.db",
		`CREATE TABLE t (id INTEGER PRIMARY KEY)`,
		`CREATE VIEW v_same AS SELECT id FROM t`,
		`CREATE VIEW v_new AS SELECT id FROM t`,
		`CREATE VIEW v_chg AS SELECT id FROM t WHERE id > 0`,
	)

	res, err := service.Compare(context.Background(), left, right)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}

	byName := map[string]service.ViewDiff{}
	for _, v := range res.Views {
		byName[v.Name] = v
	}
	if _, ok := byName["v_same"]; ok {
		t.Error("查询体相同的视图不该出现在差异清单里")
	}
	if byName["v_gone"].Status != service.CompareOnlyLeft {
		t.Errorf("v_gone 状态 = %s, 期望 only_left", byName["v_gone"].Status)
	}
	if byName["v_new"].Status != service.CompareOnlyRight {
		t.Errorf("v_new 状态 = %s, 期望 only_right", byName["v_new"].Status)
	}
	if byName["v_chg"].Status != service.CompareChanged {
		t.Errorf("v_chg 状态 = %s, 期望 changed", byName["v_chg"].Status)
	}
	if byName["v_gone"].Left == "" || byName["v_new"].Right == "" {
		t.Error("单侧视图也要带上原文供人眼对照")
	}

	upgrade := strings.Join(res.Upgrade, "\n")
	if !strings.Contains(upgrade, `DROP VIEW IF EXISTS "v_gone"`) {
		t.Errorf("升级脚本应删掉左侧多出的视图:\n%s", upgrade)
	}
	if !strings.Contains(upgrade, "CREATE VIEW v_new") {
		t.Errorf("升级脚本应建出右侧新增的视图:\n%s", upgrade)
	}

	downgrade := strings.Join(res.Downgrade, "\n")
	if !strings.Contains(downgrade, "CREATE VIEW v_gone") {
		t.Errorf("回退脚本应把删掉的视图建回来:\n%s", downgrade)
	}
	if !strings.Contains(downgrade, `DROP VIEW IF EXISTS "v_new"`) {
		t.Errorf("回退脚本应删掉新建的视图:\n%s", downgrade)
	}

	review := strings.Join(res.NeedReview, "\n")
	if !strings.Contains(review, "v_chg") || !strings.Contains(review, "视图定义有差异") {
		t.Errorf("改视图要进 need_review（SQLite 没有 CREATE OR REPLACE）: %s", review)
	}
}
