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
		}

		if syn.LineSeparatorGO && t.IsKw("GO") && aloneOnLine(script, t, true) {
			flushTo(t.Pos)
			cur = lineEnd(script, t.End)
			prev, blockMode = nil, false
			continue
		}
		if syn.LineSeparatorSlash && t.Kind == TokOp && t.Text == "/" && aloneOnLine(script, t, false) {
			flushTo(t.Pos)
			cur = lineEnd(script, t.End)
			prev, blockMode = nil, false
			continue
		}

		if consumed, end, ok := matchDelimiter(toks, i, delim); ok {
			if blockMode && delim == ";" && !(prev != nil && prev.IsKw("END")) {
				prev = &toks[i]
				continue
			}
			flushTo(t.Pos)
			cur = end
			i += consumed - 1
			prev, blockMode = nil, false
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
