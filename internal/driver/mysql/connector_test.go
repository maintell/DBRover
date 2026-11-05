package mysql_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/drivertest"
	"github.com/maintell/DBRover/internal/driver/mysql"
	"github.com/maintell/DBRover/internal/model"
)

// 集成测试的连接串，形如：
//
//	DBRover_TEST_MYSQL=root:testpass@192.168.2.88:13306
//
// 未设置时整组跳过，保证没有数据库的机器上 `go test ./...` 仍然全绿。
type testTarget struct {
	user, pass, host string
	port             int
}

func targetFromEnv(t *testing.T) testTarget {
	t.Helper()
	raw := os.Getenv("DBRover_TEST_MYSQL")
	if raw == "" {
		t.Skip("未设置 DBRover_TEST_MYSQL，跳过 MySQL 集成测试")
	}

	at := strings.LastIndex(raw, "@")
	if at <= 0 {
		t.Fatalf("DBRover_TEST_MYSQL 格式应为 user:pass@host:port，实际 %q", raw)
	}
	cred, addr := raw[:at], raw[at+1:]

	parts := strings.SplitN(cred, ":", 2)
	if len(parts) != 2 {
		t.Fatalf("凭据部分应为 user:pass，实际 %q", cred)
	}
	host, portStr, err := splitHostPort(addr)
	if err != nil {
		t.Fatalf("地址部分非法: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("端口非法: %v", err)
	}
	return testTarget{user: parts[0], pass: parts[1], host: host, port: port}
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i <= 0 || i == len(addr)-1 {
		return "", "", fmt.Errorf("缺少端口: %q", addr)
	}
	return addr[:i], addr[i+1:], nil
}

// factory 每次调用建一个独立的临时库，测试结束后统一删除。
func factory(t *testing.T, tgt testTarget) drivertest.Config {
	t.Helper()
	ctx := context.Background()
	var created []string

	base := driver.ConnectConfig{
		Host: tgt.host, Port: tgt.port,
		User: tgt.user, Password: tgt.pass,
		Environment: model.EnvTest,
	}

	t.Cleanup(func() {
		boot := base
		boot.AccessMode = model.AccessReadWrite
		c := mysql.New()
		if err := c.Open(context.Background(), boot); err != nil {
			return
		}
		if s, err := c.Acquire(context.Background()); err == nil {
			for _, name := range created {
				_, _ = s.Execute(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`", nil)
			}
			_ = s.Close()
		}
		_ = c.Close()
	})

	seq := 0
	return drivertest.Config{
		Engine: model.EngineMySQL,
		New: func(readOnly bool) (driver.Connector, error) {
			seq++
			name := fmt.Sprintf("dbrover_t_%d_%d", os.Getpid(), seq)

			// 先建库（需要一个不指定 catalog 的连接）。
			boot := base
			boot.AccessMode = model.AccessReadWrite
			bc := mysql.New()
			if err := bc.Open(ctx, boot); err != nil {
				return nil, err
			}
			bs, err := bc.Acquire(ctx)
			if err != nil {
				_ = bc.Close()
				return nil, err
			}
			if _, err := bs.Execute(ctx, "CREATE DATABASE `"+name+"`", nil); err != nil {
				_ = bs.Close()
				_ = bc.Close()
				return nil, fmt.Errorf("建测试库 %s: %w", name, err)
			}
			_ = bs.Close()
			_ = bc.Close()
			created = append(created, name)

			cfg := base
			cfg.Catalog = name
			if readOnly {
				cfg.AccessMode = model.AccessReadOnly
			} else {
				cfg.AccessMode = model.AccessReadWrite
			}
			c := mysql.New()
			if err := c.Open(ctx, cfg); err != nil {
				return nil, err
			}
			return c, nil
		},
		// MySQL 的触发器体允许是一条普通语句，不需要 DELIMITER。
		Trigger: func(ctx context.Context, s driver.Session, table string) (string, error) {
			const name = "trg_probe"
			_, err := s.Execute(ctx,
				"CREATE TRIGGER "+name+" BEFORE INSERT ON "+table+" FOR EACH ROW SET @dbrover_trg = 1", nil)
			return name, err
		},
		Procedure: func(ctx context.Context, s driver.Session) (string, []string, error) {
			const name = "proc_probe"
			// 带 BEGIN … END 体：顺带验证切分器不会把体内的分号切开。
			_, err := s.Execute(ctx,
				"CREATE PROCEDURE "+name+"() BEGIN SELECT 1; END", nil)
			return name, []string{name, "SELECT 1"}, err
		},
		DropProcedure: func(ctx context.Context, s driver.Session, name string) error {
			_, err := s.Execute(ctx, "DROP PROCEDURE IF EXISTS "+name, nil)
			return err
		},
		SlowQuery: "SELECT SLEEP(3)",
		Partitions: func(ctx context.Context, s driver.Session) (string, []string, error) {
			_, err := s.Execute(ctx,
				"CREATE TABLE dbv_part (id int, n int) PARTITION BY RANGE (n) "+
					"(PARTITION p0 VALUES LESS THAN (10), PARTITION p1 VALUES LESS THAN (20))", nil)
			return "dbv_part", []string{"p0", "p1"}, err
		},
		Comment: func(ctx context.Context, s driver.Session, table, text string) error {
			_, err := s.Execute(ctx, "ALTER TABLE "+table+" COMMENT = '"+text+"'", nil)
			return err
		},
	}
}

func TestMySQLDriverSuite(t *testing.T) {
	tgt := targetFromEnv(t)
	drivertest.Run(t, factory(t, tgt))
}

func TestNeutralTypeMySQL(t *testing.T) {
	cases := []struct {
		raw  string
		want model.TypeKind
	}{
		{"int(11)", model.TypeInteger},
		{"bigint(20) unsigned", model.TypeInteger},
		{"tinyint(1)", model.TypeInteger}, // 刻意不猜布尔
		{"decimal(10,2)", model.TypeDecimal},
		{"double", model.TypeFloat},
		{"varchar(64)", model.TypeString},
		{"text", model.TypeString},
		{"blob", model.TypeBinary},
		{"varbinary(16)", model.TypeBinary},
		{"datetime", model.TypeTimestamp},
		{"timestamp", model.TypeTimestamp},
		{"date", model.TypeDate},
		{"time", model.TypeTime},
		{"json", model.TypeJSON},
		{"enum('a','b')", model.TypeEnum},
		{"point", model.TypeGeometry},
	}
	for _, c := range cases {
		if got := mysql.NeutralType(c.raw).Kind; got != c.want {
			t.Errorf("NeutralType(%q).Kind = %s, 期望 %s", c.raw, got, c.want)
		}
	}

	if ct := mysql.NeutralType("timestamp"); !ct.WithTZ {
		t.Error("timestamp 应标记带时区")
	}
	if ct := mysql.NeutralType("datetime"); ct.WithTZ {
		t.Error("datetime 不应标记带时区")
	}
}

func TestOpenRequiresHost(t *testing.T) {
	c := mysql.New()
	if err := c.Open(context.Background(), driver.ConnectConfig{}); err == nil {
		t.Fatal("缺少主机应报错")
	}
}
