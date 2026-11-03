package dialect

import (
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		name   string
		engine model.Engine
		in     string
		want   string
	}{
		{
			name:   "数字字面量替换",
			engine: model.EnginePostgres,
			in:     "SELECT * FROM t WHERE id = 123",
			want:   "SELECT * FROM t WHERE id = ?",
		},
		{
			name:   "关键字大写标识符保留",
			engine: model.EnginePostgres,
			in:     "select Foo from Bar",
			want:   "SELECT Foo FROM Bar",
		},
		{
			name:   "注释丢弃",
			engine: model.EnginePostgres,
			in:     "SELECT 1 /* c */ FROM t",
			want:   "SELECT ? FROM t",
		},
		{
			name:   "IN 列表折叠",
			engine: model.EnginePostgres,
			in:     "SELECT * FROM t WHERE id IN (1,2,3)",
			want:   "SELECT * FROM t WHERE id IN (?+)",
		},
		{
			name:   "位置参数替换",
			engine: model.EnginePostgres,
			in:     "SELECT * FROM t WHERE id = $1",
			want:   "SELECT * FROM t WHERE id = ?",
		},
		{
			name:   "MySQL 反引号保留",
			engine: model.EngineMySQL,
			in:     "select `A` from `T`",
			want:   "SELECT `A` FROM `T`",
		},
		{
			name:   "字符串字面量替换",
			engine: model.EnginePostgres,
			in:     "SELECT * FROM t WHERE name = 'x;y'",
			want:   "SELECT * FROM t WHERE name = ?",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := For(c.engine).Normalize(c.in)
			if err != nil {
				t.Fatalf("Normalize error: %v", err)
			}
			if got != c.want {
				t.Errorf("\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

func TestFingerprintStability(t *testing.T) {
	d := For(model.EnginePostgres)

	same := [][2]string{
		{"SELECT * FROM t WHERE id = 1", "SELECT * FROM t WHERE id = 999"},
		{"SELECT * FROM t WHERE id IN (1,2)", "SELECT * FROM t WHERE id IN (5,6,7,8)"},
		{"select 1 from t", "SELECT 1\nFROM t"},
	}
	for _, p := range same {
		h1, err1 := d.Fingerprint(p[0])
		h2, err2 := d.Fingerprint(p[1])
		if err1 != nil || err2 != nil {
			t.Fatalf("Fingerprint error: %v %v", err1, err2)
		}
		if h1 != h2 {
			t.Errorf("应视为同一指纹\n  %q -> %s\n  %q -> %s", p[0], h1, p[1], h2)
		}
	}

	diff := [][2]string{
		{"SELECT * FROM t", "SELECT * FROM u"},
		{"SELECT a FROM t", "SELECT b FROM t"},
	}
	for _, p := range diff {
		h1, _ := d.Fingerprint(p[0])
		h2, _ := d.Fingerprint(p[1])
		if h1 == h2 {
			t.Errorf("不应视为同一指纹: %q / %q", p[0], p[1])
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		engine     model.Engine
		in         string
		wantClass  Class
		wantWrites bool
		wantVerb   string
		wantObject string
	}{
		{"只读查询", model.EnginePostgres, "SELECT 1", ClassQuery, false, "SELECT", ""},
		{"SHOW", model.EngineMySQL, "SHOW TABLES", ClassQuery, false, "SHOW", ""},
		{"WITH 只读", model.EnginePostgres, "WITH x AS (SELECT 1) SELECT * FROM x", ClassQuery, false, "SELECT", ""},
		{"WITH 内含写", model.EnginePostgres, "WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x", ClassQuery, true, "SELECT", ""},
		{"INSERT", model.EnginePostgres, "INSERT INTO t VALUES (1)", ClassDML, true, "INSERT", ""},
		{"UPDATE", model.EnginePostgres, "UPDATE t SET a=1", ClassDML, true, "UPDATE", ""},
		{"DELETE", model.EnginePostgres, "DELETE FROM t", ClassDML, true, "DELETE", ""},
		{"CREATE TABLE", model.EnginePostgres, "CREATE TABLE t (a int)", ClassDDL, true, "CREATE", "TABLE"},
		{"CREATE OR REPLACE VIEW", model.EnginePostgres, "CREATE OR REPLACE VIEW v AS SELECT 1", ClassDDL, true, "CREATE", "VIEW"},
		{"DROP INDEX", model.EnginePostgres, "DROP INDEX i", ClassDDL, true, "DROP", "INDEX"},
		{"TRUNCATE", model.EngineMySQL, "TRUNCATE TABLE t", ClassDDL, true, "TRUNCATE", "TABLE"},
		{"GRANT", model.EnginePostgres, "GRANT SELECT ON t TO u", ClassDCL, true, "GRANT", ""},
		{"BEGIN", model.EnginePostgres, "BEGIN", ClassTCL, false, "BEGIN", ""},
		{"无法识别按写处理", model.EnginePostgres, "CALL p()", ClassUnknown, true, "CALL", ""},
		{"EXPLAIN 只读", model.EnginePostgres, "EXPLAIN SELECT 1", ClassQuery, false, "SELECT", ""},
		{"EXPLAIN QUERY PLAN 只读", model.EngineSQLite, "EXPLAIN QUERY PLAN SELECT * FROM t", ClassQuery, false, "SELECT", ""},
		{"EXPLAIN ANALYZE 写语句", model.EnginePostgres, "EXPLAIN ANALYZE DELETE FROM t", ClassDML, true, "DELETE", ""},
		{"SELECT INTO", model.EngineMSSQL, "SELECT * INTO newtab FROM t", ClassQuery, true, "SELECT", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := For(c.engine).Analyze(c.in)
			if err != nil {
				t.Fatalf("Analyze error: %v", err)
			}
			if a.Class != c.wantClass {
				t.Errorf("Class = %s, 期望 %s", a.Class, c.wantClass)
			}
			if a.Writes != c.wantWrites {
				t.Errorf("Writes = %v, 期望 %v", a.Writes, c.wantWrites)
			}
			if a.Verb != c.wantVerb {
				t.Errorf("Verb = %q, 期望 %q", a.Verb, c.wantVerb)
			}
			if c.wantObject != "" && a.Object != c.wantObject {
				t.Errorf("Object = %q, 期望 %q", a.Object, c.wantObject)
			}
		})
	}
}

func TestAnalyzeCondition(t *testing.T) {
	cases := []struct {
		name           string
		in             string
		wantHasWhere   bool
		wantAlwaysTrue bool
		wantEquals     []string
		wantIn         []string
		wantLimit      int
	}{
		{"无 WHERE", "UPDATE t SET a=1", false, false, nil, nil, -1},
		{"主键等值", "UPDATE t SET a=1 WHERE id = 5", true, false, []string{"id"}, nil, -1},
		{"限定名主键", "UPDATE t SET a=1 WHERE t.id = 5", true, false, []string{"id"}, nil, -1},
		{"IN 主键", "DELETE FROM t WHERE id IN (1,2)", true, false, nil, []string{"id"}, -1},
		{"恒真 1=1", "DELETE FROM t WHERE 1=1", true, true, nil, nil, -1},
		{"恒真 TRUE", "DELETE FROM t WHERE TRUE", true, true, nil, nil, -1},
		{"BETWEEN 不被 AND 切断", "DELETE FROM t WHERE id BETWEEN 1 AND 9", true, false, nil, nil, -1},
		{"LIMIT", "DELETE FROM t WHERE id=1 LIMIT 1", true, false, []string{"id"}, nil, 1},
		// 回归：顶层 OR/NOT 时不得采集等值列，也不得判恒真（除非字面量恒真）。
		{"OR 恒真", "DELETE FROM t WHERE id=1 OR 1=1", true, true, nil, nil, -1},
		{"OR 非恒真", "DELETE FROM t WHERE id=5 OR name='x'", true, false, nil, nil, -1},
		{"NOT 不采集等值", "DELETE FROM t WHERE NOT id=5", true, false, nil, nil, -1},
		{"EXISTS 不误判恒真", "DELETE FROM t WHERE EXISTS (SELECT 1 FROM u)", true, false, nil, nil, -1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := For(model.EnginePostgres).Analyze(c.in)
			if err != nil {
				t.Fatalf("Analyze error: %v", err)
			}
			if a.HasWhere != c.wantHasWhere {
				t.Errorf("HasWhere = %v, 期望 %v", a.HasWhere, c.wantHasWhere)
			}
			if a.Condition.AlwaysTrue != c.wantAlwaysTrue {
				t.Errorf("AlwaysTrue = %v, 期望 %v", a.Condition.AlwaysTrue, c.wantAlwaysTrue)
			}
			if !equalStrings(a.Condition.Equals, c.wantEquals) {
				t.Errorf("Equals = %v, 期望 %v", a.Condition.Equals, c.wantEquals)
			}
			if !equalStrings(a.Condition.In, c.wantIn) {
				t.Errorf("In = %v, 期望 %v", a.Condition.In, c.wantIn)
			}
			if a.Limit != c.wantLimit {
				t.Errorf("Limit = %d, 期望 %d", a.Limit, c.wantLimit)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestQuoteIdent(t *testing.T) {
	cases := []struct {
		name   string
		engine model.Engine
		in     string
		want   string
	}{
		{"PG 双引号转义", model.EnginePostgres, `a"b`, `"a""b"`},
		{"MySQL 反引号转义", model.EngineMySQL, "a`b", "`a``b`"},
		{"MSSQL 方括号转义", model.EngineMSSQL, "a]b", "[a]]b]"},
		{"Oracle 大写保留", model.EngineOracle, "ABC", `"ABC"`},
		{"SQLite 双引号", model.EngineSQLite, "a b", `"a b"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := For(c.engine).QuoteIdent(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestQuoteQualified(t *testing.T) {
	got := For(model.EnginePostgres).QuoteQualified("public", "order")
	want := `"public"."order"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := For(model.EnginePostgres).QuoteQualified("", "t"); got != `"t"` {
		t.Errorf("空段应被跳过, got %q", got)
	}
}
