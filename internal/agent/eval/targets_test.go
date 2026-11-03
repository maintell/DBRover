package eval

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/mysql"
	"github.com/maintell/DBRover/internal/driver/pg"
	"github.com/maintell/DBRover/internal/model"
)

// evalDBName 是 MySQL 侧专用库名 / PG 侧专用 schema 名。
//
// 刻意与业务库分开：这套用例会 DROP TABLE，跑在别人的库上是灾难。
// 每次运行先重建这个库，跑完删掉。
const evalDBName = "dbrover_eval"

// targets 返回本次能跑的引擎。
//
// SQLite 永远跑；MySQL / PG 只在设了对应环境变量时才跑，且各自建一个专用库。
// 没设就跳过——CI 里没有真实实例，不能假装覆盖了三引擎。
func targets(t *testing.T) []Target {
	t.Helper()
	out := []Target{OpenSQLite(t)}
	if dsn := os.Getenv("DBRover_TEST_MYSQL"); dsn != "" {
		if tgt, ok := openMySQL(t, dsn); ok {
			out = append(out, tgt)
		}
	}
	if dsn := os.Getenv("DBRover_TEST_PG"); dsn != "" {
		if tgt, ok := openPostgres(t, dsn); ok {
			out = append(out, tgt)
		}
	}
	return out
}

// parseDSN 解析 user:pass@host:port。
func parseDSN(t *testing.T, raw string) (user, pass, host string, port int) {
	t.Helper()
	at := strings.LastIndex(raw, "@")
	if at <= 0 {
		t.Fatalf("环境变量格式应为 user:pass@host:port，实际 %q", raw)
	}
	cred, addr := raw[:at], raw[at+1:]
	parts := strings.SplitN(cred, ":", 2)
	if len(parts) != 2 {
		t.Fatalf("凭据部分应为 user:pass，实际 %q", cred)
	}
	colon := strings.LastIndex(addr, ":")
	if colon <= 0 {
		t.Fatalf("地址部分缺少端口: %q", addr)
	}
	n := 0
	for _, r := range addr[colon+1:] {
		if r < '0' || r > '9' {
			t.Fatalf("端口非法: %q", addr[colon+1:])
		}
		n = n*10 + int(r-'0')
	}
	return parts[0], parts[1], addr[:colon], n
}

// openMySQL 在专用库里打开连接，并把库重建一次。
//
// 先连默认库执行 CREATE DATABASE，再连进专用库——MySQL 的 CREATE DATABASE
// 不能在选定库里跑（其实可以，但显式写在默认库里更清楚）。
func openMySQL(t *testing.T, dsn string) (Target, bool) {
	t.Helper()
	user, pass, host, port := parseDSN(t, dsn)
	ctx := context.Background()

	boot := mysql.New()
	if err := boot.Open(ctx, driver.ConnectConfig{
		Host: host, Port: port, User: user, Password: pass,
		AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
	}); err != nil {
		t.Logf("MySQL 不可用，跳过：%v", err)
		return Target{}, false
	}
	defer func() { _ = boot.Close() }()

	if err := execOn(ctx, boot, "DROP DATABASE IF EXISTS "+evalDBName); err != nil {
		t.Logf("清理 %s 失败，跳过 MySQL：%v", evalDBName, err)
		return Target{}, false
	}
	if err := execOn(ctx, boot, "CREATE DATABASE "+evalDBName+" CHARACTER SET utf8mb4"); err != nil {
		t.Logf("建库失败，跳过 MySQL：%v", err)
		return Target{}, false
	}

	conn := mysql.New()
	if err := conn.Open(ctx, driver.ConnectConfig{
		Host: host, Port: port, User: user, Password: pass,
		Catalog: evalDBName,
		// 用例里带中文（「余额不足」），库与连接都按 utf8mb4 走。
		AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
	}); err != nil {
		t.Logf("连进 %s 失败，跳过 MySQL：%v", evalDBName, err)
		return Target{}, false
	}
	t.Cleanup(func() {
		_ = conn.Close()
		// 跑完把专用库删掉，不给下一次留脏数据。
		if b := mysql.New(); b.Open(ctx, driver.ConnectConfig{
			Host: host, Port: port, User: user, Password: pass,
			AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
		}) == nil {
			_ = execOn(ctx, b, "DROP DATABASE IF EXISTS "+evalDBName)
			_ = b.Close()
		}
	})
	return Target{Engine: model.EngineMySQL, Conn: conn}, true
}

// openPostgres 在专用 schema 里打开连接。
func openPostgres(t *testing.T, dsn string) (Target, bool) {
	t.Helper()
	user, pass, host, port := parseDSN(t, dsn)
	ctx := context.Background()

	boot := pg.New()
	if err := boot.Open(ctx, driver.ConnectConfig{
		Host: host, Port: port, User: user, Password: pass,
		AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
	}); err != nil {
		t.Logf("PostgreSQL 不可用，跳过：%v", err)
		return Target{}, false
	}
	defer func() { _ = boot.Close() }()

	for _, q := range []string{
		"DROP SCHEMA IF EXISTS " + evalDBName + " CASCADE",
		"CREATE SCHEMA " + evalDBName,
	} {
		if err := execOn(ctx, boot, q); err != nil {
			t.Logf("准备 schema 失败（%s），跳过 PG：%v", q, err)
			return Target{}, false
		}
	}

	conn := pg.New()
	if err := conn.Open(ctx, driver.ConnectConfig{
		Host: host, Port: port, User: user, Password: pass,
		Schema: evalDBName, AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
	}); err != nil {
		t.Logf("连进 schema %s 失败，跳过 PG：%v", evalDBName, err)
		return Target{}, false
	}
	t.Cleanup(func() {
		_ = conn.Close()
		if b := pg.New(); b.Open(ctx, driver.ConnectConfig{
			Host: host, Port: port, User: user, Password: pass,
			AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
		}) == nil {
			_ = execOn(ctx, b, "DROP SCHEMA IF EXISTS "+evalDBName+" CASCADE")
			_ = b.Close()
		}
	})
	return Target{Engine: model.EnginePostgres, Conn: conn}, true
}

// execOn 直接执行一条 DDL（不经过执行门：这是测试准备，不是被测路径）。
func execOn(ctx context.Context, c driver.Connector, sqlText string) error {
	s, err := c.Acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	_, err = s.Execute(ctx, sqlText, nil)
	return err
}
