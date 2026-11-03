package service

import (
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestBoundsFor 覆盖收窄预检的条件生成。
// 这条逻辑决定了「哪些类型需要探测」以及「探测条件长什么样」，
// 一旦写错，要么漏掉越界数据，要么拼出无法执行的 SQL。
func TestBoundsFor(t *testing.T) {
	const col = `"c"`

	cases := []struct {
		typeRaw string
		want    bool
		contain string
	}{
		{"INTEGER", true, `"c" < -2147483648`},
		{"int", true, `"c" > 2147483647`},
		{"SMALLINT", true, "-32768"},
		{"tinyint", true, "-128"},
		{"mediumint", true, "8388607"},
		{"VARCHAR(8)", true, `LENGTH("c") > 8`},
		{"varchar( 8 )", true, `LENGTH("c") > 8`},
		{"CHAR(4)", true, `LENGTH("c") > 4`},
		{"character varying(12)", true, `LENGTH("c") > 12`},
		{"NUMERIC(5,2)", true, `ABS("c") >= 1000`},
		{"decimal(4)", true, `ABS("c") >= 10000`},
		// 没有明确容量上限的类型不该被探测：探测条件写错比不探测更危险。
		{"TEXT", false, ""},
		{"BIGINT", false, ""},
		{"BOOLEAN", false, ""},
		{"DATE", false, ""},
		{"JSON", false, ""},
		// 形态不认识时也不探测，让引擎自己报错。
		{"VARCHAR(abc)", false, ""},
		{"NUMERIC(3,9)", false, ""},
	}

	for _, c := range cases {
		t.Run(c.typeRaw, func(t *testing.T) {
			check, ok := boundsFor(c.typeRaw, col)
			if ok != c.want {
				t.Fatalf("boundsFor(%q) ok = %v, 期望 %v", c.typeRaw, ok, c.want)
			}
			if !c.want {
				return
			}
			if !strings.Contains(check.cond, c.contain) {
				t.Errorf("条件 %q 不含 %q", check.cond, c.contain)
			}
			if check.why == "" {
				t.Error("应当给出人类可读的原因")
			}
			// 条件里不允许出现分号或注释：它的值全部是数字，不该带任何可执行片段。
			if strings.ContainsAny(check.cond, ";") || strings.Contains(check.cond, "--") {
				t.Errorf("条件里出现了可疑片段: %q", check.cond)
			}
		})
	}
}

// TestSafeDefaultText 验证默认值文本的兜底：带分号的一律不还原；
// 裸字符串按列类型补引号（MySQL 的 information_schema 给的就是裸值）。
func TestSafeDefaultText(t *testing.T) {
	imported := "now()"
	bad := "1; DROP TABLE t"
	blank := "  "
	bare := "d"
	quoted := "'d'::character varying"
	num := "42"

	if got := safeDefaultText(model.TypeString, nil); got != "" {
		t.Errorf("nil 应返回空串, 实际 %q", got)
	}
	if got := safeDefaultText(model.TypeTimestamp, &imported); got != "now()" {
		t.Errorf("合法默认值被丢掉: %q", got)
	}
	if got := safeDefaultText(model.TypeString, &bad); got != "" {
		t.Errorf("带分号的默认值必须丢弃, 实际 %q", got)
	}
	if got := safeDefaultText(model.TypeString, &blank); got != "" {
		t.Errorf("空白默认值应返回空串, 实际 %q", got)
	}
	// 这一条是真实踩到的：MySQL 回 `d`，拼进 MODIFY COLUMN 就是语法错误。
	if got := safeDefaultText(model.TypeString, &bare); got != "'d'" {
		t.Errorf("裸字符串默认值应补引号, 实际 %q", got)
	}
	if got := safeDefaultText(model.TypeString, &quoted); got != "'d'::character varying" {
		t.Errorf("已带引号的默认值不该再包一层, 实际 %q", got)
	}
	if got := safeDefaultText(model.TypeInteger, &num); got != "42" {
		t.Errorf("数字默认值不该被加引号, 实际 %q", got)
	}
}
