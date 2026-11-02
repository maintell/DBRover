package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/drivertest"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
)

// factory 每次返回一个全新的数据库文件，便于验证 DDL 回放。
func factory(t *testing.T) drivertest.Config {
	t.Helper()
	return drivertest.Config{
		Engine: model.EngineSQLite,
		New: func(readOnly bool) (driver.Connector, error) {
			path := filepath.Join(t.TempDir(), "db.sqlite")
			ctx := context.Background()
			cfg := driver.ConnectConfig{
				Name:        "test",
				Host:        path,
				AccessMode:  model.AccessReadWrite,
				Environment: model.EnvLocal,
			}

			if readOnly {
				// 只读连接要求文件已存在，先建一个表把文件落盘。
				seed := sqlite.New()
				if err := seed.Open(ctx, cfg); err != nil {
					return nil, err
				}
				s, err := seed.Acquire(ctx)
				if err != nil {
					_ = seed.Close()
					return nil, err
				}
				if _, err := s.Execute(ctx, "CREATE TABLE seed (id INTEGER)", nil); err != nil {
					_ = s.Close()
					_ = seed.Close()
					return nil, err
				}
				_ = s.Close()
				_ = seed.Close()
				cfg.AccessMode = model.AccessReadOnly
			}

			c := sqlite.New()
			if err := c.Open(ctx, cfg); err != nil {
				return nil, err
			}
			return c, nil
		},
	}
}

func TestSQLiteDriverSuite(t *testing.T) {
	drivertest.Run(t, factory(t))
}

func TestNeutralType(t *testing.T) {
	cases := []struct {
		raw  string
		want model.TypeKind
	}{
		{"INTEGER", model.TypeInteger},
		{"BIGINT", model.TypeInteger},
		{"POINT", model.TypeInteger}, // 含 INT，按 SQLite affinity 规则归为 INTEGER
		{"VARCHAR(32)", model.TypeString},
		{"TEXT", model.TypeString},
		{"CLOB", model.TypeString},
		{"BLOB", model.TypeBinary},
		{"", model.TypeBinary},
		{"REAL", model.TypeFloat},
		{"DOUBLE", model.TypeFloat},
		{"BOOLEAN", model.TypeBoolean},
		{"DATE", model.TypeDate},
		{"TIMESTAMP", model.TypeTimestamp},
		{"NUMERIC", model.TypeDecimal},
	}
	for _, c := range cases {
		if got := sqlite.NeutralType(c.raw).Kind; got != c.want {
			t.Errorf("NeutralType(%q).Kind = %s, 期望 %s", c.raw, got, c.want)
		}
	}
}

func TestOpenRequiresPath(t *testing.T) {
	c := sqlite.New()
	if err := c.Open(context.Background(), driver.ConnectConfig{}); err == nil {
		t.Fatal("缺少路径时应返回错误")
	}
}

func TestReadOnlyRequiresExistingFile(t *testing.T) {
	c := sqlite.New()
	err := c.Open(context.Background(), driver.ConnectConfig{
		Host:       filepath.Join(t.TempDir(), "missing.sqlite"),
		AccessMode: model.AccessReadOnly,
	})
	if err == nil {
		t.Fatal("只读打开不存在的文件应返回错误")
	}
}
