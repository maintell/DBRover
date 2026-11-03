package dialect

import (
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestCrossDatabase 锁住 docs/05 验收 12 的判据：只有 PostgreSQL 的三段名
// 是跨库引用；字符串、注释、数字里的点都不算。
func TestCrossDatabase(t *testing.T) {
	cases := []struct {
		name    string
		engine  model.Engine
		sql     string
		wantRef string
		want    bool
	}{
		{"PG 三段名", model.EnginePostgres, `SELECT * FROM other.public.users`, "other.public.users", true},
		{"PG 连接里出现", model.EnginePostgres, `SELECT u.id FROM other.public.users u JOIN public.t t ON t.id = u.id`, "other.public.users", true},
		{"PG 引用带引号", model.EnginePostgres, `SELECT * FROM "my db"."public"."t"`, `"my db"."public"."t"`, true},
		{"PG 四段名整体报出来", model.EnginePostgres, `SELECT * FROM a.b.c.d`, "a.b.c.d", true},
		{"PG 函数调用也算跨库", model.EnginePostgres, `SELECT other.public.now()`, "other.public.now", true},

		{"PG 两段名正常", model.EnginePostgres, `SELECT * FROM public.users`, "", false},
		{"PG 单段名正常", model.EnginePostgres, `SELECT * FROM users`, "", false},
		{"PG 字符串里的点不算", model.EnginePostgres, `SELECT 'other.public.users' AS s`, "", false},
		{"PG 注释里的点不算", model.EnginePostgres, "-- other.public.users\nSELECT 1", "", false},
		{"PG 块注释里的点不算", model.EnginePostgres, "/* other.public.users */ SELECT 1", "", false},
		{"PG 数字里的点不算", model.EnginePostgres, `SELECT 1.2.3`, "", false},
		{"PG 别名加列", model.EnginePostgres, `SELECT t.col FROM public.t t`, "", false},

		{"MySQL 两段名是正常跨库", model.EngineMySQL, `SELECT * FROM other.users`, "", false},
		{"MySQL 三段名不拦", model.EngineMySQL, `SELECT * FROM a.b.c`, "", false},
		{"SQL Server 三段名正常", model.EngineMSSQL, `SELECT * FROM db.dbo.t`, "", false},
		{"Oracle 三段名是 dblink", model.EngineOracle, `SELECT * FROM a.b.c`, "", false},
		{"SQLite 不拦", model.EngineSQLite, `SELECT * FROM other.public.t`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, ok := For(c.engine).CrossDatabase(c.sql)
			if ok != c.want {
				t.Fatalf("命中 = %v，期望 %v（ref=%q）", ok, c.want, ref)
			}
			if ref != c.wantRef {
				t.Errorf("ref = %q，期望 %q", ref, c.wantRef)
			}
		})
	}
}
