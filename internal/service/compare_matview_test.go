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

// openPGCatalogSide 打开 PG 上指定库（不存在就建），用于比对需要「两侧」的场景。
//
// 先删后建：上一轮失败留下的物化视图会让这次的结论不可信。
func openPGCatalogSide(t *testing.T, tgt rowsTarget, catalog string) driver.Connector {
	t.Helper()
	ctx := context.Background()

	boot := tgt.newConn()
	if err := boot.Open(ctx, driver.ConnectConfig{
		Host: tgt.host, Port: tgt.port, User: tgt.user, Password: tgt.pass,
		Catalog: "postgres", AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
	}); err != nil {
		t.Fatalf("连接 postgres: %v", err)
	}
	s, err := boot.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, "DROP DATABASE IF EXISTS "+catalog, nil); err != nil {
		t.Fatalf("删库 %s: %v", catalog, err)
	}
	if _, err := s.Execute(ctx, "CREATE DATABASE "+catalog, nil); err != nil {
		t.Fatalf("建库 %s: %v", catalog, err)
	}
	_ = s.Close()
	_ = boot.Close()

	c := tgt.newConn()
	if err := c.Open(ctx, driver.ConnectConfig{
		Host: tgt.host, Port: tgt.port, User: tgt.user, Password: tgt.pass,
		Catalog: catalog, AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
	}); err != nil {
		t.Fatalf("连接 %s: %v", catalog, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// execOn 在给定连接上依次执行语句。
func execOn(t *testing.T, ctx context.Context, c driver.Connector, stmts ...string) {
	t.Helper()
	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	for _, q := range stmts {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("执行 %q: %v", q, err)
		}
	}
}

// TestCompareMatViewsOnRealPG 验证物化视图参与比对，且用的 DROP 是它自己的那一条。
//
// 物化视图与普通视图在比对里必须分开：`DROP VIEW` 删不动物化视图，而

// `DROP MATERIALIZED VIEW` 用在普通视图上是语法错。这条用例把两类混在一起跑，
// 就是为了让「用错关键字」当场红。
func TestCompareMatViewsOnRealPG(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		tgt := tgt
		if tgt.name != "pg" {
			continue // 只有 PG 有物化视图；不能在循环里 t.Skip，会把整条用例结束掉
		}
		t.Run("pg", func(t *testing.T) {
			ctx := context.Background()
			left := openPGCatalogSide(t, tgt, "dbrover_cmp_mv_l")
			right := openPGCatalogSide(t, tgt, "dbrover_cmp_mv_r")

			base := []string{
				"CREATE TABLE mt (id integer PRIMARY KEY, v integer)",
				"INSERT INTO mt VALUES (1, 10), (2, 20)",
			}
			execOn(t, ctx, left, base...)
			execOn(t, ctx, right, base...)
			execOn(t, ctx, left,
				"CREATE MATERIALIZED VIEW mv_same AS SELECT id FROM mt",
				"CREATE MATERIALIZED VIEW mv_gone AS SELECT id FROM mt",
				"CREATE MATERIALIZED VIEW mv_chg AS SELECT id FROM mt",
			)
			execOn(t, ctx, right,
				"CREATE MATERIALIZED VIEW mv_same AS SELECT id FROM mt",
				"CREATE MATERIALIZED VIEW mv_new AS SELECT id FROM mt",
				"CREATE MATERIALIZED VIEW mv_chg AS SELECT id FROM mt WHERE id > 0",
			)

			res, err := service.Compare(ctx, left, right)
			if err != nil {
				t.Fatalf("Compare: %v", err)
			}
			byName := map[string]service.ViewDiff{}
			for _, v := range res.Views {
				byName[v.Name] = v
			}
			if _, ok := byName["mv_same"]; ok {
				t.Error("查询体相同的物化视图不该出现在差异清单里")
			}
			if d := byName["mv_gone"]; d.Status != service.CompareOnlyLeft {
				t.Errorf("mv_gone 状态 = %s, 期望 only_left", d.Status)
			} else if d.Kind != model.KindMatView {
				t.Errorf("mv_gone 的 kind = %q, 期望 matview", d.Kind)
			}
			if d := byName["mv_new"]; d.Status != service.CompareOnlyRight {
				t.Errorf("mv_new 状态 = %s, 期望 only_right", d.Status)
			}
			if d := byName["mv_chg"]; d.Status != service.CompareChanged {
				t.Errorf("mv_chg 状态 = %s, 期望 changed", d.Status)
			}

			// 脚本必须用物化视图自己的关键字。
			upgrade := strings.Join(res.Upgrade, "\n")
			if !strings.Contains(upgrade, "DROP MATERIALIZED VIEW") {
				t.Errorf("升级脚本里应有 DROP MATERIALIZED VIEW：%s", upgrade)
			}
			if strings.Contains(upgrade, "DROP VIEW") {
				t.Errorf("升级脚本用错了关键字（DROP VIEW 删不动物化视图）：%s", upgrade)
			}
			if !strings.Contains(upgrade, "CREATE MATERIALIZED VIEW") {
				t.Errorf("升级脚本里应有 CREATE MATERIALIZED VIEW：%s", upgrade)
			}

			// 定义有差异时要提醒「重建会重算并覆盖已物化的数据」。
			review := strings.Join(res.NeedReview, "\n")
			if !strings.Contains(review, "REFRESH") && !strings.Contains(review, "重建") {
				t.Errorf("差异说明里应提到重建/刷新的代价：%s", review)
			}
		})
	}
}

// TestCompareMatViewsUnsupportedSide 验证「另一侧引擎没有物化视图」时不出脚本。
//
// 否则会生成一条左侧方言的 DROP，而右侧根本没有这个概念——那条语句在右侧
// 永远执行不了，看起来却像是「升级可以一键完成」。
func TestCompareMatViewsUnsupportedSide(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		tgt := tgt
		if tgt.name != "pg" {
			continue
		}
		t.Run("pg-vs-sqlite", func(t *testing.T) {
			ctx := context.Background()
			left := openPGCatalogSide(t, tgt, "dbrover_cmp_mv_l")
			execOn(t, ctx, left,
				"CREATE TABLE mt (id integer PRIMARY KEY)",
				"CREATE MATERIALIZED VIEW mv_only_here AS SELECT id FROM mt",
			)

			path := filepath.Join(t.TempDir(), "noright.db")
			right := sqlite.New()
			if err := right.Open(ctx, driver.ConnectConfig{
				Host: path, AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
			}); err != nil {
				t.Fatalf("打开 sqlite: %v", err)
			}
			defer func() { _ = right.Close() }()

			res, err := service.Compare(ctx, left, right)
			if err != nil {
				t.Fatalf("Compare: %v", err)
			}
			for _, v := range res.Views {
				if v.Name == "mv_only_here" {
					t.Errorf("右侧没有物化视图这个概念，不该进差异清单：%+v", v)
				}
			}
			if up := strings.Join(res.Upgrade, "\n"); strings.Contains(up, "MATERIALIZED") {
				t.Errorf("右侧不支持物化视图时不该生成升级脚本：%s", up)
			}
			if !strings.Contains(strings.Join(res.NeedReview, "\n"), "mv_only_here") {
				t.Errorf("应把它记进 need_review 并点名：%v", res.NeedReview)
			}
		})
	}
}
