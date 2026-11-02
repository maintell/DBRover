package pg

import (
	"strconv"
	"strings"

	"github.com/maintell/DBRover/internal/driver/dialect"
)

// rewritePlaceholders 把 `?` 占位符改写成 PostgreSQL 的 `$1, $2 ...`。
//
// 为什么在连接器里做：DBRover 对上层承诺「统一用 ? 传参」，
// 引擎自己的写法由连接器适配。用户从 SQLite / MySQL 切过来时
// 不该被迫改写所有 SQL；而直接写 $1 的语句原样透传，不做二次改写。
//
// 只改词法上的参数记号：字符串与注释里的 ? 不会被当成占位符。
func rewritePlaceholders(sqlText string) string {
	if !strings.Contains(sqlText, "?") {
		return sqlText
	}
	toks, err := dialect.Tokenize(sqlText, dialect.SyntaxFor("postgres"))
	if err != nil {
		// 词法失败就让引擎去报原始错误，不要在这里制造第二层误导。
		return sqlText
	}

	var b strings.Builder
	b.Grow(len(sqlText) + 8)
	last := 0
	n := 0
	for _, t := range toks {
		if t.Pos > last {
			b.WriteString(sqlText[last:t.Pos])
		}
		if t.Kind == dialect.TokParam && t.Text == "?" {
			n++
			b.WriteString("$")
			b.WriteString(strconv.Itoa(n))
		} else {
			b.WriteString(t.Text)
		}
		last = t.End
	}
	if last < len(sqlText) {
		b.WriteString(sqlText[last:])
	}
	if n == 0 {
		return sqlText
	}
	return b.String()
}
