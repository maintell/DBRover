package pg

import "testing"

func TestRewritePlaceholders(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "两个位置参数",
			in:   "SELECT * FROM t WHERE a = ? AND b = ?",
			want: "SELECT * FROM t WHERE a = $1 AND b = $2",
		},
		{
			name: "字符串里的问号不动",
			in:   "SELECT '?' AS q FROM t WHERE id = ?",
			want: "SELECT '?' AS q FROM t WHERE id = $1",
		},
		{
			name: "注释里的问号不动",
			in:   "-- 这是问号 ?\nSELECT ?",
			want: "-- 这是问号 ?\nSELECT $1",
		},
		{
			name: "原生 $1 语法透传",
			in:   "SELECT * FROM t WHERE id = $1",
			want: "SELECT * FROM t WHERE id = $1",
		},
		{
			name: "INSERT",
			in:   "INSERT INTO t VALUES (?, ?)",
			want: "INSERT INTO t VALUES ($1, $2)",
		},
		{
			name: "无占位符原样返回",
			in:   "SELECT 1",
			want: "SELECT 1",
		},
		{
			name: "引用标识符里的问号不动",
			in:   `SELECT "a?b" FROM t WHERE id = ?`,
			want: `SELECT "a?b" FROM t WHERE id = $1`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rewritePlaceholders(c.in); got != c.want {
				t.Errorf("\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}
