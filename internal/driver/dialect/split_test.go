package dialect

import (
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

func splitTexts(t *testing.T, e model.Engine, script string) []string {
	t.Helper()
	sts, err := For(e).Split(script)
	if err != nil {
		t.Fatalf("Split(%q) 返回错误: %v", script, err)
	}
	out := make([]string, len(sts))
	for i, s := range sts {
		out[i] = s.Text
	}
	return out
}

func TestSplit(t *testing.T) {
	cases := []struct {
		name   string
		engine model.Engine
		in     string
		want   []string
	}{
		{
			name:   "简单两句",
			engine: model.EnginePostgres,
			in:     "SELECT 1; SELECT 2;",
			want:   []string{"SELECT 1", "SELECT 2"},
		},
		{
			name:   "单引号字符串内的分号",
			engine: model.EnginePostgres,
			in:     "SELECT 'a;b'; SELECT 2",
			want:   []string{"SELECT 'a;b'", "SELECT 2"},
		},
		{
			name:   "双写单引号转义",
			engine: model.EnginePostgres,
			in:     "SELECT 'it''s; ok'; SELECT 1",
			want:   []string{"SELECT 'it''s; ok'", "SELECT 1"},
		},
		{
			name:   "双引号标识符内的分号",
			engine: model.EnginePostgres,
			in:     `SELECT "a;b" FROM t; SELECT 1`,
			want:   []string{`SELECT "a;b" FROM t`, "SELECT 1"},
		},
		{
			name:   "反引号标识符内的分号",
			engine: model.EngineMySQL,
			in:     "SELECT `a;b` FROM t; SELECT 1",
			want:   []string{"SELECT `a;b` FROM t", "SELECT 1"},
		},
		{
			name:   "方括号标识符内的分号",
			engine: model.EngineMSSQL,
			in:     "SELECT [a;b] FROM t; SELECT 1",
			want:   []string{"SELECT [a;b] FROM t", "SELECT 1"},
		},
		{
			name:   "行注释内的分号",
			engine: model.EnginePostgres,
			in:     "SELECT 1 -- a;b\n; SELECT 2",
			want:   []string{"SELECT 1 -- a;b", "SELECT 2"},
		},
		{
			name:   "块注释内的分号",
			engine: model.EnginePostgres,
			in:     "SELECT 1 /* ; */; SELECT 2",
			want:   []string{"SELECT 1 /* ; */", "SELECT 2"},
		},
		{
			name:   "嵌套块注释（PG）",
			engine: model.EnginePostgres,
			in:     "SELECT 1 /* a /* b; */ c */; SELECT 2",
			want:   []string{"SELECT 1 /* a /* b; */ c */", "SELECT 2"},
		},
		{
			name:   "MySQL 井号注释",
			engine: model.EngineMySQL,
			in:     "SELECT 1 # a;b\n; SELECT 2",
			want:   []string{"SELECT 1 # a;b", "SELECT 2"},
		},
		{
			name:   "MySQL 反斜杠转义",
			engine: model.EngineMySQL,
			in:     `SELECT 'a\'; b' AS x`,
			want:   []string{`SELECT 'a\'; b' AS x`},
		},
		{
			name:   "PG dollar-quote",
			engine: model.EnginePostgres,
			in:     "CREATE FUNCTION f() RETURNS int AS $$ BEGIN; RETURN 1; END; $$ LANGUAGE plpgsql;",
			want:   []string{"CREATE FUNCTION f() RETURNS int AS $$ BEGIN; RETURN 1; END; $$ LANGUAGE plpgsql"},
		},
		{
			name:   "PG 带标签 dollar-quote",
			engine: model.EnginePostgres,
			in:     "SELECT $tag$ a;b $tag$",
			want:   []string{"SELECT $tag$ a;b $tag$"},
		},
		{
			name:   "MySQL DELIMITER 存储过程",
			engine: model.EngineMySQL,
			in:     "DELIMITER $$\nCREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\n  SELECT 2;\nEND$$\nDELIMITER ;\nSELECT 3;",
			want:   []string{"CREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\n  SELECT 2;\nEND", "SELECT 3"},
		},
		{
			name:   "MSSQL GO 分隔符",
			engine: model.EngineMSSQL,
			in:     "SELECT 1\nGO\nSELECT 2\nGO\n",
			want:   []string{"SELECT 1", "SELECT 2"},
		},
		{
			name:   "MSSQL GO 带次数",
			engine: model.EngineMSSQL,
			in:     "SELECT 1\nGO 3\nSELECT 2",
			want:   []string{"SELECT 1", "SELECT 2"},
		},
		{
			name:   "Oracle PL/SQL 块以斜杠结束",
			engine: model.EngineOracle,
			in:     "BEGIN\n  NULL;\nEND;\n/\nSELECT 1 FROM dual;",
			want:   []string{"BEGIN\n  NULL;\nEND", "SELECT 1 FROM dual"},
		},
		{
			name:   "去首尾空白",
			engine: model.EnginePostgres,
			in:     "  \n SELECT 1  \n ",
			want:   []string{"SELECT 1"},
		},
		{
			name:   "MySQL 存储过程不带 DELIMITER 也按块切",
			engine: model.EngineMySQL,
			in:     "CREATE PROCEDURE p()\nBEGIN\n  DECLARE x INT;\n  SET x = 1;\n  IF x = 1 THEN\n    SET x = 2;\n  END IF;\n  INSERT INTO t (id) VALUES (x);\nEND;\nSELECT 1;",
			want: []string{
				"CREATE PROCEDURE p()\nBEGIN\n  DECLARE x INT;\n  SET x = 1;\n  IF x = 1 THEN\n    SET x = 2;\n  END IF;\n  INSERT INTO t (id) VALUES (x);\nEND",
				"SELECT 1",
			},
		},
		{
			name:   "存储过程里嵌套 BEGIN 不会提前收尾",
			engine: model.EngineMySQL,
			in:     "CREATE PROCEDURE p2()\nBEGIN\n  BEGIN\n    SET @a = 1;\n  END;\n  SET @b = 2;\nEND;",
			want: []string{
				"CREATE PROCEDURE p2()\nBEGIN\n  BEGIN\n    SET @a = 1;\n  END;\n  SET @b = 2;\nEND",
			},
		},
		{
			name:   "MySQL 函数体同样按块处理",
			engine: model.EngineMySQL,
			in:     "CREATE FUNCTION f() RETURNS INT\nBEGIN\n  RETURN 1;\nEND;",
			want:   []string{"CREATE FUNCTION f() RETURNS INT\nBEGIN\n  RETURN 1;\nEND"},
		},
		{
			name:   "没有 BEGIN 的 CREATE 照常按分号切",
			engine: model.EngineMySQL,
			in:     "CREATE TABLE a (id INT); CREATE TABLE b (id INT);",
			want:   []string{"CREATE TABLE a (id INT)", "CREATE TABLE b (id INT)"},
		},
		{
			name:   "纯空白无语句",
			engine: model.EnginePostgres,
			in:     "   \n\t ",
			want:   nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitTexts(t, c.engine, c.in)
			if len(got) != len(c.want) {
				t.Fatalf("语句数 = %d, 期望 %d\ngot=%q", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("stmt[%d]\n got: %q\nwant: %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestSplitUnterminatedString(t *testing.T) {
	if _, err := For(model.EnginePostgres).Split("SELECT 'abc"); err == nil {
		t.Fatal("未闭合字符串应返回错误")
	}
}

func TestSplitStatementPositions(t *testing.T) {
	script := "SELECT 1;\nSELECT 22;"
	sts, err := For(model.EnginePostgres).Split(script)
	if err != nil {
		t.Fatal(err)
	}
	if len(sts) != 2 {
		t.Fatalf("语句数 = %d", len(sts))
	}
	if sts[1].Line != 2 {
		t.Errorf("第二条语句行号 = %d, 期望 2", sts[1].Line)
	}
	if script[sts[1].Start:sts[1].End] != "SELECT 22" {
		t.Errorf("偏移不匹配: %q", script[sts[1].Start:sts[1].End])
	}
	if sts[1].Index != 1 {
		t.Errorf("Index = %d", sts[1].Index)
	}
}

func TestTokenizeBasics(t *testing.T) {
	toks, err := Tokenize("SELECT 'a''b', 0x1F, $1, :name, @v", SyntaxFor("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	toks = FilterComments(toks)

	var strings_, nums, params int
	for _, tk := range toks {
		switch tk.Kind {
		case TokString:
			strings_++
			if tk.Value != "a'b" {
				t.Errorf("字符串解码 = %q, 期望 a'b", tk.Value)
			}
		case TokNumber:
			nums++
		case TokParam:
			params++
		}
	}
	if strings_ != 1 || nums != 1 || params != 3 {
		t.Errorf("string=%d number=%d param=%d, 期望 1/1/3", strings_, nums, params)
	}
}

func TestTokenizeBracketEscape(t *testing.T) {
	toks, err := Tokenize("SELECT [a]]b]", SyntaxFor("mssql"))
	if err != nil {
		t.Fatal(err)
	}
	var quoted string
	for _, tk := range toks {
		if tk.Kind == TokQuoted {
			quoted = tk.Value
		}
	}
	if quoted != "a]b" {
		t.Errorf("方括号解码 = %q, 期望 a]b", quoted)
	}
}
