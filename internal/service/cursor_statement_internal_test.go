package service

import (
	"testing"

	"github.com/maintell/DBRover/internal/driver/dialect"
)

// TestStatementAt 锁住「光标落在哪就执行哪条」的边界（docs/05 验收 1、2）。
func TestStatementAt(t *testing.T) {
	// 三段文本："AAA" (0..2) 、"BBB" (5..7)、"CCC" (10..12)
	stmts := []dialect.Statement{
		{Index: 1, Start: 0, End: 3},
		{Index: 2, Start: 5, End: 8},
		{Index: 3, Start: 10, End: 13},
	}
	cases := []struct {
		name   string
		cursor int
		want   int
	}{
		{"第一条内部", 1, 0},
		{"第一条开头", 0, 0},
		{"两条之间靠前", 3, 0},
		{"两条之间靠后", 4, 0}, // 空白处仍归前一条：写完一句回车，光标在下一行
		{"第二条开头", 5, 1},
		{"第二条内部", 6, 1},
		{"第二三条之间", 9, 1},
		{"第三条内部", 11, 2},
		{"末尾之后", 999, 2},
	}
	for _, c := range cases {
		got := statementAt(stmts, c.cursor)
		if got.Index != stmts[c.want].Index {
			t.Errorf("%s：cursor=%d 选中第 %d 条，期望第 %d 条", c.name, c.cursor, got.Index, stmts[c.want].Index)
		}
	}

	// 光标在第一句之前也不能空手而归。
	shifted := []dialect.Statement{{Index: 7, Start: 4, End: 9}}
	if got := statementAt(shifted, 0); got.Index != 7 {
		t.Errorf("光标在首句之前应回落到第一句，实际第 %d 条", got.Index)
	}
}
