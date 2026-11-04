package dialect

import (
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestSplitTriggerBody 守住「触发器体内的分号不分句」。
//
// SQLite 的触发器体必然是 BEGIN ... END; 且内部至少有一个分号，早先的切分器
// 会把它切成两段（`CREATE TRIGGER ... BEGIN INSERT ...` 与 `END`），于是
// 在 SQL 工作区里建触发器 100% 失败。
func TestSplitTriggerBody(t *testing.T) {
	cases := []struct {
		name   string
		engine model.Engine
		script string
		want   []string
	}{
		{
			name:   "体内多个分号",
			engine: model.EngineSQLite,
			script: "CREATE TRIGGER trg AFTER INSERT ON t BEGIN INSERT INTO audit (n) VALUES ('a'); UPDATE x SET y = 1; END;\nSELECT 1;",
			want: []string{
				"CREATE TRIGGER trg AFTER INSERT ON t BEGIN INSERT INTO audit (n) VALUES ('a'); UPDATE x SET y = 1; END",
				"SELECT 1",
			},
		},
		{
			name:   "体内 CASE ... END 不算体结束",
			engine: model.EngineSQLite,
			script: "CREATE TRIGGER trg AFTER INSERT ON t BEGIN INSERT INTO audit (n) VALUES (CASE WHEN 1=1 THEN 'a' ELSE 'b' END); END;",
			want: []string{
				"CREATE TRIGGER trg AFTER INSERT ON t BEGIN INSERT INTO audit (n) VALUES (CASE WHEN 1=1 THEN 'a' ELSE 'b' END); END",
			},
		},
		{
			name:   "普通多语句不受影响",
			engine: model.EngineSQLite,
			script: "SELECT 1; SELECT 2;",
			want:   []string{"SELECT 1", "SELECT 2"},
		},
		{
			name:   "没有体的触发器照常切",
			engine: model.EngineMySQL,
			script: "CREATE TRIGGER t1 BEFORE INSERT ON t FOR EACH ROW SET @x = 1; SELECT 3;",
			want: []string{
				"CREATE TRIGGER t1 BEFORE INSERT ON t FOR EACH ROW SET @x = 1",
				"SELECT 3",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := For(c.engine).Split(c.script)
			if err != nil {
				t.Fatalf("Split: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("切出 %d 段，期望 %d 段：%+v", len(got), len(c.want), texts(got))
			}
			for i := range c.want {
				if got[i].Text != c.want[i] {
					t.Errorf("第 %d 段 = %q，期望 %q", i, got[i].Text, c.want[i])
				}
			}
		})
	}
}

func texts(st []Statement) []string {
	out := make([]string, len(st))
	for i, s := range st {
		out[i] = s.Text
	}
	return out
}
