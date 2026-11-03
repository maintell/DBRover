package dialect

import (
	"strings"

	"github.com/maintell/DBRover/internal/model"
)

// CrossDatabase 检测语句里的「跨库引用」，返回引用原文（如 `other.public.t`）。
//
// 只有 PostgreSQL 需要这条：它一个连接只对应一个库，三段名 `库.模式.表`
// 的第一段是 catalog，引擎必然报 "cross-database references are not
// implemented"。与其把引擎的原始报错甩给用户，不如在执行前说清楚。
//
// 其余引擎的三段名都是正常写法，刻意不拦：
//   - MySQL 的 `库.表` 就是跨库（同实例内合法）
//   - SQL Server 的 `库.模式.表` 合法
//   - Oracle 的 `模式.表` 合法，`库.模式.表` 是 dblink
//
// 只在词法层找 `名字 . 名字 . 名字`，字符串与注释里的内容不算——那些是数据，
// 不是标识符。
func (d impl) CrossDatabase(sqlText string) (string, bool) {
	if d.engine != model.EnginePostgres {
		return "", false
	}
	toks, err := Tokenize(sqlText, d.Syntax())
	if err != nil {
		// 词法都不通就不是我们要管的：交给执行门按原有规则拒绝。
		return "", false
	}
	toks = FilterComments(toks)

	isName := func(t Token) bool { return t.Kind == TokIdent || t.Kind == TokQuoted }
	isDot := func(t Token) bool { return t.Kind == TokPunct && t.Text == "." }

	for i := 0; i+4 < len(toks); i++ {
		if !isName(toks[i]) || !isDot(toks[i+1]) || !isName(toks[i+2]) ||
			!isDot(toks[i+3]) || !isName(toks[i+4]) {
			continue
		}
		// 四段以上的名字把后面的段一起收进来，报出来的才是用户写的那个。
		parts := []string{toks[i].Text, toks[i+2].Text, toks[i+4].Text}
		for j := i + 5; j+1 < len(toks) && isDot(toks[j]) && isName(toks[j+1]); j += 2 {
			parts = append(parts, toks[j+1].Text)
		}
		return strings.Join(parts, "."), true
	}
	return "", false
}
