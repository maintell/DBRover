package api

import (
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestConnectConfigSeparatesCatalogFromSchema 锁住一条曾经写错的映射：
// 原实现把 Catalog 同时赋给 Schema，PG 上会让所有查表落到不存在的 schema。
func TestConnectConfigSeparatesCatalogFromSchema(t *testing.T) {
	req := createConnectionRequest{
		Name: "pg", Engine: "pg", Host: "192.168.2.88", Port: 15432,
		Catalog: "dbrover_test", User: "postgres", Password: "secret",
	}
	cfg := connectConfigFrom(req, model.EnvTest, model.AccessReadOnly)

	if cfg.Catalog != "dbrover_test" {
		t.Errorf("Catalog = %q, 期望 dbrover_test", cfg.Catalog)
	}
	if cfg.Schema != "" {
		t.Errorf("未指定 schema 时应留空交给引擎默认值，实际 %q", cfg.Schema)
	}
	if cfg.Port != 15432 || cfg.User != "postgres" {
		t.Errorf("网络参数没透传: port=%d user=%q", cfg.Port, cfg.User)
	}

	// 显式指定 schema 时要原样透传。
	req.Schema = "biz"
	if got := connectConfigFrom(req, model.EnvTest, model.AccessReadOnly); got.Schema != "biz" {
		t.Errorf("显式 schema 应透传，实际 %q", got.Schema)
	}
}

// TestConnectConfigSQLiteIgnoresNetworkFields 验证 SQLite 只认文件路径：
// 误填端口/账号不该被带进去，否则驱动会拿它当地址解析。
func TestConnectConfigSQLiteIgnoresNetworkFields(t *testing.T) {
	req := createConnectionRequest{
		Name: "local", Engine: "sqlite", Host: "F:\\tmp\\a.db",
		Port: 3306, Catalog: "x", Schema: "y", User: "root", Password: "pw",
	}
	cfg := connectConfigFrom(req, model.EnvLocal, model.AccessReadWrite)

	if cfg.Host != "F:\\tmp\\a.db" {
		t.Errorf("Host = %q, 期望文件路径", cfg.Host)
	}
	if cfg.Port != 0 || cfg.Catalog != "" || cfg.Schema != "" || cfg.User != "" || cfg.Password != "" {
		t.Errorf("SQLite 不该带网络字段: %+v", cfg)
	}
}
