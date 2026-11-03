package service

import "testing"

// TestNormalizeTypeText 锁住「纯写法差异不算差异」这一条：
// 跨引擎比对时，同一类型在两边打印出的文本天然不同，不归一就是稳定假阳性。
func TestNormalizeTypeText(t *testing.T) {
	cases := []struct {
		a, b     string
		wantSame bool
	}{
		// 同义写法：PostgreSQL 的 FormatType 与 MySQL 的 COLUMN_TYPE
		{"character varying(32)", "varchar(32)", true},
		{"integer", "int", true},
		{"BIGINT(20)", "bigint", true},
		{"int(11)", "int", true},
		{"  INTEGER  ", "int", true},
		{"VARCHAR(32)", "varchar(32)", true},

		// 精度与宽度是真差异，不能吃掉
		{"decimal(10,2)", "decimal(12,2)", false},
		{"numeric(38,10)", "numeric(38,2)", false},
		{"varchar(32)", "varchar(64)", false},
		// 显示宽度之外的修饰不能假装没看见
		{"int(11) unsigned", "int", false},

		// 不同类型
		{"bigint", "text", false},
		{"datetime", "timestamp", false},
	}
	for _, c := range cases {
		gotA := normalizeTypeText(c.a)
		gotB := normalizeTypeText(c.b)
		same := gotA == gotB
		if same != c.wantSame {
			t.Errorf("normalizeTypeText(%q)=%q vs (%q)=%q, 期望相同=%v", c.a, gotA, c.b, gotB, c.wantSame)
		}
	}
}
