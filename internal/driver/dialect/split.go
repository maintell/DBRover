package dialect

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// delimDirective 是 MySQL 客户端指令 DELIMITER 的一行。
type delimDirective struct {
	lineStart int
	lineEnd   int
	delim     string
}

// Split 把脚本切分成语句。
//
// 必须正确处理：字符串与注释内的分号、PostgreSQL dollar-quote、
// MySQL 的 DELIMITER 指令、SQL Server 的 GO、Oracle 的 / 行。
func (d impl) Split(script string) ([]Statement, error) {
	toks, err := Tokenize(script, d.Syntax())
	if err != nil {
		return nil, err
	}

	syn := d.Syntax()
	dirs := scanDelimiterDirectives(script)

	var out []Statement
	delim := ";"
	cur := 0
	di := 0

	// Oracle 的 BEGIN/DECLARE ... END; 块内部的分号不是语句分隔符。
	// MSSQL 的 ; 是语句终止符，块内的分号同样是终止符，因此只对 Oracle 生效。
	var prev *Token
	blockMode := false

	// CREATE TRIGGER / PROCEDURE / FUNCTION ... BEGIN <语句>; ... END; 是另一种
	// 「体内分号不算分隔符」：SQLite 的触发器体必然带内部分号，MySQL 不写
	// DELIMITER 时存储过程也一样（DELIMITER 只是客户端语法，通过驱动发过去的
	// 是一条完整语句）。
	//
	// 靠三种计数避免误判：CASE … END（体内可以写 CASE 表达式）、嵌套 BEGIN
	// （存储过程里很常见）、END IF / END LOOP 这类块收尾。
	inRoutine := false
	bodyDepth := 0 // BEGIN 的嵌套层数；0 表示还没进块
	caseDepth := 0

	flushTo := func(end int) {
		if s, ok := makeStatement(script, cur, end, len(out)); ok {
			out = append(out, s)
		}
	}

	for i := 0; i < len(toks); i++ {
		t := toks[i]

		// 越过此 Token 的 DELIMITER 指令：先把指令前的残留结句，再切换分隔符。
		for di < len(dirs) && dirs[di].lineEnd <= t.Pos {
			flushTo(dirs[di].lineStart)
			cur = dirs[di].lineEnd
			delim = dirs[di].delim
			di++
		}

		if t.Kind == TokComment {
			continue
		}

		if prev == nil {
			blockMode = syn.LineSeparatorSlash && (t.IsKw("BEGIN") || t.IsKw("DECLARE"))
			inRoutine, bodyDepth, caseDepth = startsWithRoutineBlock(toks, i), 0, 0
		}

		if inRoutine {
			switch {
			case t.IsKw("BEGIN"):
				// 嵌套 BEGIN 要累加：存储过程里 BEGIN … END 套 BEGIN … END 很常见，
				// 只记「进没进块」会让内层的 END 提前把整条语句收掉。
				bodyDepth++
			case bodyDepth > 0 && t.IsKw("CASE"):
				caseDepth++
			case bodyDepth > 0 && t.IsKw("END"):
				// END IF / END LOOP / END WHILE / END REPEAT 是块内收尾，
				// 不是体的结束；把紧跟的关键字一起吃掉，避免它又被当成新的块开始。
				if next := i + 1; next < len(toks) && endsCompoundBlock(toks[next]) {
					// END CASE 同时要闭掉一次 CASE 计数，否则后面的 END 会被
					// 误当成 CASE 的收尾，体永远结束不了。
					if toks[next].IsKw("CASE") && caseDepth > 0 {
						caseDepth--
					}
					i++
				} else if caseDepth > 0 {
					caseDepth--
				} else {
					bodyDepth--
				}
			}
		}

		if syn.LineSeparatorGO && t.IsKw("GO") && aloneOnLine(script, t, true) {
			flushTo(t.Pos)
			cur = lineEnd(script, t.End)
			prev, blockMode = nil, false
			inRoutine, bodyDepth, caseDepth = false, 0, 0
			continue
		}
		if syn.LineSeparatorSlash && t.Kind == TokOp && t.Text == "/" && aloneOnLine(script, t, false) {
			flushTo(t.Pos)
			cur = lineEnd(script, t.End)
			prev, blockMode = nil, false
			inRoutine, bodyDepth, caseDepth = false, 0, 0
			continue
		}

		if consumed, end, ok := matchDelimiter(toks, i, delim); ok {
			if blockMode && delim == ";" && !(prev != nil && prev.IsKw("END")) {
				prev = &toks[i]
				continue
			}
			// 例程/触发器体内的分号属于体，不是语句结束。
			if inRoutine && bodyDepth > 0 {
				prev = &toks[i]
				continue
			}
			flushTo(t.Pos)
			cur = end
			i += consumed - 1
			prev, blockMode = nil, false
			inRoutine, bodyDepth, caseDepth = false, 0, 0
			continue
		}
		prev = &toks[i]
	}

	for di < len(dirs) {
		flushTo(dirs[di].lineStart)
		cur = dirs[di].lineEnd
		di++
	}
	flushTo(len(script))

	return out, nil
}

// endsCompoundBlock 判断紧跟 END 的这个关键字是不是块收尾（END IF 之类）。
func endsCompoundBlock(t Token) bool {
	return t.IsKw("IF") || t.IsKw("LOOP") || t.IsKw("WHILE") || t.IsKw("REPEAT") || t.IsKw("CASE")
}

// startsWithRoutineBlock 判断从 toks[i] 起的语句是不是带 BEGIN 体的例程/触发器。
//
// 只用于决定「体内的分号算不算分隔符」，不做完整解析——真正的语法错误交给引擎。
// 认的形态：CREATE [OR REPLACE] [TEMP|TEMPORARY] TRIGGER|PROCEDURE|FUNCTION|EVENT。
//
// 为什么不按引擎区分：PostgreSQL 的函数体是 dollar-quoted（词法层已经是**一个**
// Token，内部没有分号可切），把它也纳进来不会误判；而 MySQL 不写 DELIMITER 时
// 存储过程体与触发器体是同一套 BEGIN … END 结构。
func startsWithRoutineBlock(toks []Token, i int) bool {
	if i >= len(toks) || !toks[i].IsKw("CREATE") {
		return false
	}
	// 关键字前可能夹着修饰语：OR REPLACE / TEMP / 以及 MySQL `SHOW CREATE` 一定
	// 带的 `DEFINER=`user`@`host``。这些形态不固定（DEFINER 的值会被切成好几个
	// Token），所以不逐个枚举，而是在一个有界窗口里找第一个对象关键字。
	//
	// 遇到括号或分号就判定「不是带体的例程定义」并立刻退出：CREATE TABLE 的列
	// 清单、CREATE INDEX 的列名都会先出现括号，不会误判。
	//
	// 这条修的是一个真机 bug：`CREATE DEFINER=… PROCEDURE p() BEGIN …; END` 以前
	// 认不出来，体内分号被切成两条语句，回滚语句执行时报 1064。
	for j, n := i+1, 0; j < len(toks) && n < 16; j, n = j+1, n+1 {
		t := toks[j]
		if t.IsKw("TRIGGER") || t.IsKw("PROCEDURE") || t.IsKw("FUNCTION") || t.IsKw("EVENT") {
			return true
		}
		if t.Text == "(" || t.Text == ";" {
			return false
		}
	}
	return false
}

func makeStatement(script string, from, to, index int) (Statement, bool) {
	if from > to {
		from = to
	}
	raw := script[from:to]
	if strings.TrimSpace(raw) == "" {
		return Statement{}, false
	}
	off := from
	for off < to {
		r, size := utf8.DecodeRuneInString(script[off:])
		if !unicode.IsSpace(r) {
			break
		}
		off += size
	}
	line := 1 + strings.Count(script[:off], "\n")
	return Statement{
		Text:  strings.TrimSpace(raw),
		Start: off,
		End:   to,
		Line:  line,
		Index: index,
	}, true
}

// matchDelimiter 在 toks[i] 处尝试匹配分隔符。
// 自定义分隔符（如 $$、//、END）通过相邻 Token 原文拼接匹配。
func matchDelimiter(toks []Token, i int, delim string) (consumed int, end int, ok bool) {
	if delim == ";" {
		if toks[i].Kind == TokPunct && toks[i].Text == ";" {
			return 1, toks[i].End, true
		}
		return 0, 0, false
	}
	var b strings.Builder
	last := -1
	for j := i; j < len(toks); j++ {
		t := toks[j]
		if t.Kind == TokComment {
			return 0, 0, false
		}
		if last >= 0 && t.Pos != last {
			return 0, 0, false // 中间有空白，不是连续的分隔符
		}
		b.WriteString(t.Text)
		last = t.End
		s := b.String()
		if s == delim {
			return j - i + 1, t.End, true
		}
		if len(s) >= len(delim) || !strings.HasPrefix(delim, s) {
			return 0, 0, false
		}
	}
	return 0, 0, false
}

func scanDelimiterDirectives(script string) []delimDirective {
	var out []delimDirective
	const kw = "delimiter"
	pos := 0
	for pos < len(script) {
		end := lineEnd(script, pos)
		line := script[pos:end]
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > len(kw) && strings.EqualFold(trimmed[:len(kw)], kw) {
			if r := rune(trimmed[len(kw)]); unicode.IsSpace(r) {
				rest := strings.TrimSpace(trimmed[len(kw):])
				if rest != "" {
					out = append(out, delimDirective{lineStart: pos, lineEnd: end, delim: rest})
				}
			}
		}
		pos = end
	}
	return out
}

// aloneOnLine 判断 Token 是否独占一行（前后只有空白，可选允许尾部数字与行注释）。
func aloneOnLine(script string, t Token, allowCount bool) bool {
	ls := lineStart(script, t.Pos)
	le := lineEnd(script, t.End)

	before := strings.TrimSpace(script[ls:t.Pos])
	if before != "" {
		return false
	}

	after := script[t.End:le]
	if idx := strings.Index(after, "--"); idx >= 0 {
		after = after[:idx]
	}
	after = strings.TrimSpace(after)
	if after == "" {
		return true
	}
	if allowCount {
		for _, r := range after {
			if !unicode.IsDigit(r) {
				return false
			}
		}
		return true
	}
	return false
}

func lineStart(script string, pos int) int {
	if pos > len(script) {
		pos = len(script)
	}
	if idx := strings.LastIndexByte(script[:pos], '\n'); idx >= 0 {
		return idx + 1
	}
	return 0
}

func lineEnd(script string, pos int) int {
	if pos > len(script) {
		return len(script)
	}
	if idx := strings.IndexByte(script[pos:], '\n'); idx >= 0 {
		return pos + idx + 1
	}
	return len(script)
}
