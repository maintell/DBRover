package pg_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/drivertest"
	"github.com/maintell/DBRover/internal/driver/pg"
	"github.com/maintell/DBRover/internal/model"
)

// 集成测试连接串，形如：
//
//	DBRover_TEST_PG=postgres:testpass@192.168.2.88:15432
//
// 未设置时整组跳过。
type testTarget struct {
	user, pass, host string
	port             int
}

func targetFromEnv(t *testing.T) testTarget {
	t.Helper()
	raw := os.Getenv("DBRover_TEST_PG")
	if raw == "" {
		t.Skip("未设置 DBRover_TEST_PG，跳过 PostgreSQL 集成测试")
	}
	at := strings.LastIndex(raw, "@")
	if at <= 0 {
		t.Fatalf("DBRover_TEST_PG 格式应为 user:pass@host:port，实际 %q", raw)
	}
	cred, addr := raw[:at], raw[at+1:]
	parts := strings.SplitN(cred, ":", 2)
	if len(parts) != 2 {
		t.Fatalf("凭据部分应为 user:pass，实际 %q", cred)
	}
	i := strings.LastIndex(addr, ":")
	if i <= 0 {
		t.Fatalf("地址部分缺少端口: %q", addr)
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		t.Fatalf("端口非法: %v", err)
	}
	return testTarget{user: parts[0], pass: parts[1], host: addr[:i], port: port}
}

// factory 每次调用建一个独立数据库，测试结束统一删除。
// 用「一库一次」而不是「一模式一次」：DDL 里带 schema 限定，
// 回放时必须能在同名 public 模式下重建。
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
		boot.Catalog = "postgres"
		boot.AccessMode = model.AccessReadWrite
		c := pg.New()
		if err := c.Open(context.Background(), boot); err != nil {
			return
		}
		if s, err := c.Acquire(context.Background()); err == nil {
			for _, name := range created {
				_, _ = s.Execute(context.Background(),
					fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name), nil)
			}
			_ = s.Close()
		}
		_ = c.Close()
	})

	seq := 0
	return drivertest.Config{
		Engine:     model.EnginePostgres,
		BinaryType: "BYTEA",
		New: func(readOnly bool) (driver.Connector, error) {
			seq++
			name := fmt.Sprintf("dbrover_t_%d_%d", os.Getpid(), seq)

			boot := base
			boot.Catalog = "postgres"
			boot.AccessMode = model.AccessReadWrite
			bc := pg.New()
			if err := bc.Open(ctx, boot); err != nil {
				return nil, err
			}
			bs, err := bc.Acquire(ctx)
			if err != nil {
				_ = bc.Close()
				return nil, err
			}
			if _, err := bs.Execute(ctx, fmt.Sprintf(`CREATE DATABASE %s`, name), nil); err != nil {
				_ = bs.Close()
				_ = bc.Close()
				return nil, fmt.Errorf("建测试库 %s: %w", name, err)
			}
			_ = bs.Close()
			_ = bc.Close()
			created = append(created, name)

			cfg := base
			cfg.Catalog = name
			cfg.Schema = "public"
			if readOnly {
				cfg.AccessMode = model.AccessReadOnly
			} else {
				cfg.AccessMode = model.AccessReadWrite
			}
			c := pg.New()
			if err := c.Open(ctx, cfg); err != nil {
				return nil, err
			}
			return c, nil
		},
		// PG 的触发器要先把「触发器函数」建出来，二者是两个对象。
		Trigger: func(ctx context.Context, s driver.Session, table string) (string, error) {
			const name, fn = "trg_probe", "trg_probe_fn"
			if _, err := s.Execute(ctx,
				"CREATE FUNCTION "+fn+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$", nil); err != nil {
				return "", err
			}
			if _, err := s.Execute(ctx,
				"CREATE TRIGGER "+name+" BEFORE INSERT ON "+table+" FOR EACH ROW EXECUTE FUNCTION "+fn+"()", nil); err != nil {
				return "", err
			}
			return name, nil
		},
		Sequence: func(ctx context.Context, s driver.Session) (string, []string, error) {
			// 带非默认选项建：只有这样，「取到的定义」是否保真才有意义。
			const name = "seq_probe"
			_, err := s.Execute(ctx,
				"CREATE SEQUENCE "+name+" AS bigint INCREMENT BY 3 MINVALUE 5 MAXVALUE 9000 START WITH 7 CACHE 4", nil)
			return name, []string{
				"AS bigint", "INCREMENT BY 3", "MINVALUE 5", "MAXVALUE 9000", "START WITH 7", "CACHE 4",
			}, err
		},
		DropSequence: func(ctx context.Context, s driver.Session, name string) error {
			_, err := s.Execute(ctx, "DROP SEQUENCE "+name, nil)
			return err
		},
		Type: func(ctx context.Context, s driver.Session, name string, values []string) error {
			quoted := make([]string, len(values))
			for i, v := range values {
				quoted[i] = "'" + v + "'"
			}
			_, err := s.Execute(ctx,
				"CREATE TYPE "+name+" AS ENUM ("+strings.Join(quoted, ", ")+")", nil)
			return err
		},
		DropType: func(ctx context.Context, s driver.Session, name string) error {
			_, err := s.Execute(ctx, "DROP TYPE "+name, nil)
			return err
		},
		Comment: func(ctx context.Context, s driver.Session, table, text string) error {
			_, err := s.Execute(ctx, "COMMENT ON TABLE "+table+" IS '"+text+"'", nil)
			return err
		},
	}
}

func TestPGDriverSuite(t *testing.T) {
	tgt := targetFromEnv(t)
	drivertest.Run(t, factory(t, tgt))
}

func TestNeutralTypePG(t *testing.T) {
	cases := []struct {
		raw  string
		want model.TypeKind
	}{
		{"integer", model.TypeInteger},
		{"bigint", model.TypeInteger},
		{"numeric(10,2)", model.TypeDecimal},
		{"double precision", model.TypeFloat},
		{"character varying(64)", model.TypeString},
		{"text", model.TypeString},
		{"bytea", model.TypeBinary},
		{"boolean", model.TypeBoolean},
		{"timestamp without time zone", model.TypeTimestamp},
		{"timestamp with time zone", model.TypeTimestamp},
		{"uuid", model.TypeUUID},
		{"jsonb", model.TypeJSON},
		{"integer[]", model.TypeArray},
		{"inet", model.TypeNetwork},
	}
	for _, c := range cases {
		if got := pg.NeutralType(c.raw).Kind; got != c.want {
			t.Errorf("NeutralType(%q).Kind = %s, 期望 %s", c.raw, got, c.want)
		}
	}

	if ct := pg.NeutralType("timestamp with time zone"); !ct.WithTZ {
		t.Error("timestamptz 应标记带时区")
	}
	if ct := pg.NeutralType("integer[]"); ct.Elem == nil || ct.Elem.Kind != model.TypeInteger {
		t.Error("数组应带元素类型")
	}
}

func TestOpenRequiresHostPG(t *testing.T) {
	c := pg.New()
	if err := c.Open(context.Background(), driver.ConnectConfig{}); err == nil {
		t.Fatal("缺少主机应报错")
	}
}
