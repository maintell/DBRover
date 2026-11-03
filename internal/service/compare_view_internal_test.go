package service

import "testing"

// TestViewQueryBody 锁住「怎么从 CREATE VIEW 原文里取出查询体」。
//
// 这段逻辑最容易犯的错是把查询体里的 AS 当成定义分界（`select a AS b from t`），
// 或者被引号里的 AS 骗到（字符串字面量、名字叫 "as_view" 的视图）。
func TestViewQueryBody(t *testing.T) {
	cases := []struct {
		name string
		ddl  string
		want string
	}{
		{
			name: "SQLite 原文",
			ddl:  "CREATE VIEW v AS SELECT a, b FROM t",
			want: "SELECT a, b FROM t",
		},
		{
			name: "SQLite 带列清单",
			ddl:  "CREATE VIEW v (x, y) AS SELECT a, b FROM t",
			want: "SELECT a, b FROM t",
		},
		{
			name: "MySQL SHOW CREATE VIEW 的头部要被跳过",
			ddl:  "CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `v` AS select `a` AS `a` from `t`",
			want: "select `a` AS `a` from `t`",
		},
		{
			name: "查询体里的 AS 不能当分界",
			ddl:  "CREATE VIEW v AS SELECT a AS alias FROM t",
			want: "SELECT a AS alias FROM t",
		},
		{
			name: "PostgreSQL pg_get_viewdef 的多行原文",
			ddl:  "CREATE VIEW \"public\".\"v\" AS\n SELECT t.a,\n    t.b\n   FROM t;",
			want: "SELECT t.a,\n    t.b\n   FROM t;",
		},
		{
			name: "视图名里含 as 不能被误判",
			ddl:  `CREATE VIEW "as_view" AS SELECT 1`,
			want: "SELECT 1",
		},
		{
			name: "字符串字面量里的 AS 不算分界",
			ddl:  "CREATE VIEW v AS SELECT 'a AS b' FROM t",
			want: "SELECT 'a AS b' FROM t",
		},
		{
			name: "没有 AS 时原样返回，不猜",
			ddl:  "SELECT 1",
			want: "SELECT 1",
		},
	}
	for _, c := range cases {
		if got := viewQueryBody(c.ddl); got != c.want {
			t.Errorf("%s：viewQueryBody(%q)=%q, 期望 %q", c.name, c.ddl, got, c.want)
		}
	}
}

// TestNormalizeViewBody 只抹平空白与结尾分号，不做「聪明」的归一化。
func TestNormalizeViewBody(t *testing.T) {
	same := [][2]string{
		{"SELECT a\n  FROM t", "SELECT a FROM t"},
		{"SELECT a FROM t;", "SELECT a FROM t"},
		{"  SELECT   a   FROM   t  ", "SELECT a FROM t"},
	}
	for _, p := range same {
		if normalizeViewBody(p[0]) != normalizeViewBody(p[1]) {
			t.Errorf("%q 与 %q 应归一为同一串", p[0], p[1])
		}
	}

	// 大小写与引号是真实差异，不能被抹掉：字符串字面量大小写敏感。
	diff := [][2]string{
		{"SELECT a FROM t", "select a from t"},
		{`SELECT "a" FROM t`, "SELECT a FROM t"},
		{"SELECT 'x' FROM t", "SELECT 'X' FROM t"},
	}
	for _, p := range diff {
		if normalizeViewBody(p[0]) == normalizeViewBody(p[1]) {
			t.Errorf("%q 与 %q 不该被归一为同一串", p[0], p[1])
		}
	}
}

// TestViewReasons 明确「不知道」不等于「相同」。
func TestViewReasons(t *testing.T) {
	same := viewDef{body: "select 1"}
	other := viewDef{body: "select 2"}
	unknown := viewDef{}

	if got := viewReasons(same, same); len(got) != 0 {
		t.Errorf("查询体相同不该有差异: %v", got)
	}
	if got := viewReasons(same, other); len(got) == 0 {
		t.Error("查询体不同必须报差异")
	}
	if got := viewReasons(unknown, same); len(got) == 0 {
		t.Error("左侧取不到定义时不能判成相同")
	}
	if got := viewReasons(same, unknown); len(got) == 0 {
		t.Error("右侧取不到定义时不能判成相同")
	}
}
