package dialect

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TokKind 是词法单元类型。
type TokKind uint8

const (
	TokEOF TokKind = iota
	TokIdent
	TokQuoted
	TokString
	TokNumber
	TokParam
	TokOp
	TokPunct
	TokComment
)

func (k TokKind) String() string {
	switch k {
	case TokIdent:
		return "ident"
	case TokQuoted:
		return "quoted"
	case TokString:
		return "string"
	case TokNumber:
		return "number"
	case TokParam:
		return "param"
	case TokOp:
		return "op"
	case TokPunct:
		return "punct"
	case TokComment:
		return "comment"
	default:
		return "eof"
	}
}

// Token 是一个词法单元。Text 保留原始源码切片，Value 是解码后的值。
type Token struct {
	Kind  TokKind
	Text  string
	Value string
	Pos   int
	End   int
	Line  int
	Col   int
}

// Upper 返回 Token 的大写形式（仅对未引用标识符/关键字有意义）。
func (t Token) Upper() string {
	if t.Kind == TokIdent {
		return strings.ToUpper(t.Text)
	}
	return ""
}

// IsKw 判断是否为某个关键字（大小写不敏感，仅对未引用标识符生效）。
func (t Token) IsKw(kw string) bool {
	return t.Kind == TokIdent && strings.EqualFold(t.Text, kw)
}

// Sig 返回用于比较的签名：标识符按大小写不敏感处理。
func (t Token) Sig() string {
	switch t.Kind {
	case TokIdent:
		return strings.ToUpper(t.Text)
	case TokQuoted:
		return t.Value
	default:
		return t.Text
	}
}

// Syntax 是方言的词法开关。
type Syntax struct {
	HashComment        bool // MySQL: # 行注释
	DoubleQuoteIdent   bool // "x" 作为标识符
	BacktickIdent      bool // `x` 作为标识符
	BracketIdent       bool // [x] 作为标识符
	DollarQuote        bool // PostgreSQL $tag$...$tag$
	NestedBlockComment bool // /* /* */ */ 可嵌套
	BackslashEscape    bool // 字符串内 \' \" 转义（MySQL 默认开）
	LineSeparatorGO    bool // SQL Server 的 GO 批分隔符
	LineSeparatorSlash bool // Oracle 的 / 作为 PL/SQL 块结束
}

// SyntaxFor 返回各引擎的词法开关。
func SyntaxFor(e string) Syntax {
	switch strings.ToLower(e) {
	case "postgres":
		return Syntax{
			DoubleQuoteIdent:   true,
			DollarQuote:        true,
			NestedBlockComment: true,
		}
	case "mysql", "mariadb":
		return Syntax{
			HashComment:      true,
			DoubleQuoteIdent: true, // 仅在 ANSI_QUOTES 下是标识符，这里保守开启
			BacktickIdent:    true,
			BackslashEscape:  true,
		}
	case "mssql":
		return Syntax{
			DoubleQuoteIdent: true,
			BracketIdent:     true,
			LineSeparatorGO:  true,
		}
	case "oracle":
		return Syntax{
			DoubleQuoteIdent:   true,
			LineSeparatorSlash: true,
		}
	case "sqlite":
		return Syntax{
			DoubleQuoteIdent: true,
			BacktickIdent:    true,
			BracketIdent:     true,
		}
	default:
		return Syntax{DoubleQuoteIdent: true}
	}
}

// SyntaxError 是词法/切分错误，带位置。
type SyntaxError struct {
	Msg  string
	Line int
	Col  int
	Pos  int
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("%s (line %d, col %d)", e.Msg, e.Line, e.Col)
}

func errAt(msg string, line, col, pos int) error {
	return &SyntaxError{Msg: msg, Line: line, Col: col, Pos: pos}
}

// Tokenize 把脚本拆成词法单元。注释也作为 Token 返回，由调用方决定是否丢弃。
func Tokenize(src string, syn Syntax) ([]Token, error) {
	toks := make([]Token, 0, len(src)/4+8)
	n := len(src)
	i, line, col := 0, 1, 1

	step := func(k int) {
		end := i + k
		if end > n {
			end = n
		}
		for i < end {
			r, size := utf8.DecodeRuneInString(src[i:])
			if r == '\n' {
				line++
				col = 1
			} else {
				col++
			}
			i += size
		}
	}

	emit := func(k TokKind, start, sLine, sCol int, value string) {
		toks = append(toks, Token{
			Kind: k, Text: src[start:i], Value: value,
			Pos: start, End: i, Line: sLine, Col: sCol,
		})
	}

	for i < n {
		start, sLine, sCol := i, line, col
		c := src[i]

		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v':
			step(1)

		case c == '-' && i+1 < n && src[i+1] == '-':
			for i < n && src[i] != '\n' {
				step(1)
			}
			emit(TokComment, start, sLine, sCol, "")

		case c == '#' && syn.HashComment:
			for i < n && src[i] != '\n' {
				step(1)
			}
			emit(TokComment, start, sLine, sCol, "")

		case c == '/' && i+1 < n && src[i+1] == '*':
			step(2)
			depth := 1
			for i < n && depth > 0 {
				if syn.NestedBlockComment && i+1 < n && src[i] == '/' && src[i+1] == '*' {
					depth++
					step(2)
					continue
				}
				if i+1 < n && src[i] == '*' && src[i+1] == '/' {
					depth--
					step(2)
					continue
				}
				step(1)
			}
			if depth > 0 {
				return nil, errAt("unterminated block comment", sLine, sCol, start)
			}
			emit(TokComment, start, sLine, sCol, "")

		case c == '\'':
			step(1)
			var b strings.Builder
			closed := false
			for i < n {
				ch := src[i]
				if syn.BackslashEscape && ch == '\\' && i+1 < n {
					step(1)
					switch src[i] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					case 'r':
						b.WriteByte('\r')
					case '0':
						b.WriteByte(0)
					case 'b':
						b.WriteByte('\b')
					case 'Z':
						b.WriteByte(0x1a)
					default:
						b.WriteByte(src[i])
					}
					step(1)
					continue
				}
				if ch == '\'' {
					if i+1 < n && src[i+1] == '\'' {
						b.WriteByte('\'')
						step(2)
						continue
					}
					step(1)
					closed = true
					break
				}
				r, size := utf8.DecodeRuneInString(src[i:])
				b.WriteRune(r)
				step(size)
			}
			if !closed {
				return nil, errAt("unterminated string literal", sLine, sCol, start)
			}
			emit(TokString, start, sLine, sCol, b.String())

		case c == '"' && syn.DoubleQuoteIdent:
			v, ok, err := scanDelimitedQuoted(src, &i, &line, &col, '"', '"', sLine, sCol, start)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errAt("unterminated quoted identifier", sLine, sCol, start)
			}
			emit(TokQuoted, start, sLine, sCol, v)

		case c == '`' && syn.BacktickIdent:
			v, ok, err := scanDelimitedQuoted(src, &i, &line, &col, '`', '`', sLine, sCol, start)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errAt("unterminated quoted identifier", sLine, sCol, start)
			}
			emit(TokQuoted, start, sLine, sCol, v)

		case c == '[' && syn.BracketIdent:
			v, ok, err := scanDelimitedQuoted(src, &i, &line, &col, '[', ']', sLine, sCol, start)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errAt("unterminated bracketed identifier", sLine, sCol, start)
			}
			emit(TokQuoted, start, sLine, sCol, v)

		case c == '$':
			if syn.DollarQuote {
				if tag, ok := dollarTagAt(src, i); ok {
					tagLen := len(tag)
					step(tagLen)
					bodyStart := i
					idx := strings.Index(src[i:], tag)
					if idx < 0 {
						return nil, errAt("unterminated dollar-quoted string", sLine, sCol, start)
					}
					step(idx)
					body := src[bodyStart : bodyStart+idx]
					step(tagLen)
					emit(TokString, start, sLine, sCol, body)
					continue
				}
			}
			if i+1 < n && isDigit(src[i+1]) {
				step(1)
				for i < n && isDigit(src[i]) {
					step(1)
				}
				emit(TokParam, start, sLine, sCol, src[start:i])
			} else {
				step(1)
				emit(TokOp, start, sLine, sCol, "$")
			}

		case isDigit(c) || (c == '.' && i+1 < n && isDigit(src[i+1])):
			if c == '0' && i+1 < n && (src[i+1] == 'x' || src[i+1] == 'X') {
				step(2)
				for i < n && isHex(src[i]) {
					step(1)
				}
			} else {
				for i < n && isDigit(src[i]) {
					step(1)
				}
				if i < n && src[i] == '.' {
					step(1)
					for i < n && isDigit(src[i]) {
						step(1)
					}
				}
				if i < n && (src[i] == 'e' || src[i] == 'E') {
					j := i + 1
					if j < n && (src[j] == '+' || src[j] == '-') {
						j++
					}
					if j < n && isDigit(src[j]) {
						step(j - i)
						for i < n && isDigit(src[i]) {
							step(1)
						}
					}
				}
			}
			emit(TokNumber, start, sLine, sCol, src[start:i])

		case c == '?':
			step(1)
			emit(TokParam, start, sLine, sCol, "?")

		case c == ':' && i+1 < n && isIdentStartByte(src[i+1]):
			step(1)
			for i < n && isIdentPartByte(src[i]) {
				step(1)
			}
			emit(TokParam, start, sLine, sCol, src[start:i])

		case c == '@' && i+1 < n && (isIdentStartByte(src[i+1]) || src[i+1] == '@'):
			step(1)
			if i < n && src[i] == '@' {
				step(1)
			}
			for i < n && isIdentPartByte(src[i]) {
				step(1)
			}
			emit(TokParam, start, sLine, sCol, src[start:i])

		default:
			r, size := utf8.DecodeRuneInString(src[i:])
			if isIdentStartRune(r) {
				step(size)
				for i < n {
					// $ 是合法标识符字符，但当它开启 $$ 或 $tag$ 时必须断开：
					// 否则 MySQL 的 DELIMITER $$ 会被吞进前一个标识符（如 END$$）。
					if src[i] == '$' && startsDollarQuote(src, i, syn) {
						break
					}
					r2, s2 := utf8.DecodeRuneInString(src[i:])
					if !isIdentPartRune(r2) {
						break
					}
					step(s2)
				}
				emit(TokIdent, start, sLine, sCol, src[start:i])
				continue
			}
			if strings.ContainsRune("(),;.[]", r) {
				step(size)
				emit(TokPunct, start, sLine, sCol, src[start:i])
				continue
			}
			matched := false
			for _, op := range operators {
				if strings.HasPrefix(src[i:], op) {
					step(len(op))
					emit(TokOp, start, sLine, sCol, op)
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			step(size)
			emit(TokOp, start, sLine, sCol, src[start:i])
		}
	}

	return toks, nil
}

// scanDelimitedQuoted 扫描被成对定界符包裹的标识符，支持双写转义（"" / “ / ]]）。
func scanDelimitedQuoted(src string, i, line, col *int, open, close byte, sLine, sCol, start int) (string, bool, error) {
	n := len(src)
	step := func(k int) {
		end := *i + k
		if end > n {
			end = n
		}
		for *i < end {
			r, size := utf8.DecodeRuneInString(src[*i:])
			if r == '\n' {
				*line++
				*col = 1
			} else {
				*col++
			}
			*i += size
		}
	}
	step(1)
	var b strings.Builder
	for *i < n {
		c := src[*i]
		if c == close {
			if *i+1 < n && src[*i+1] == close {
				b.WriteByte(close)
				step(2)
				continue
			}
			step(1)
			return b.String(), true, nil
		}
		r, size := utf8.DecodeRuneInString(src[*i:])
		b.WriteRune(r)
		step(size)
	}
	return "", false, errAt("unterminated quoted identifier", sLine, sCol, start)
}

// startsDollarQuote 判断 src[i] 处的 $ 是否开启一个 dollar-quote 序列。
// 无论方言是否支持 dollar-quote，$$ 都必须断开标识符扫描，
// 因为 MySQL 客户端指令 DELIMITER $$ 依赖这一行为。
func startsDollarQuote(src string, i int, syn Syntax) bool {
	if i >= len(src) || src[i] != '$' {
		return false
	}
	if i+1 < len(src) && src[i+1] == '$' {
		return true
	}
	if !syn.DollarQuote {
		return false
	}
	_, ok := dollarTagAt(src, i)
	return ok
}

// dollarTagAt 匹配 $tag$ 形式，返回含两端 $ 的完整标签。
func dollarTagAt(src string, i int) (string, bool) {
	n := len(src)
	if i >= n || src[i] != '$' {
		return "", false
	}
	j := i + 1
	for j < n && (src[j] == '_' || isLetterByte(src[j]) || isDigit(src[j])) {
		if j == i+1 && isDigit(src[j]) {
			return "", false
		}
		j++
	}
	if j < n && src[j] == '$' {
		return src[i : j+1], true
	}
	return "", false
}

var operators = []string{
	"->>", "#>>", "!~~*", "~~*", "!~~", "!~*",
	"::", "#>", "->", "<>", "!=", ">=", "<=", "||", ":=", "=>", "<=>", "!~", "~*",
	"=", "<", ">", "+", "-", "*", "/", "%", "&", "|", "^", "~", "!", "#",
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isLetterByte(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isIdentStartByte(c byte) bool { return isLetterByte(c) || c == '_' }

func isIdentPartByte(c byte) bool { return isLetterByte(c) || isDigit(c) || c == '_' || c == '$' }

func isIdentStartRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || r > 127 && unicode.IsLetter(r)
}

func isIdentPartRune(r rune) bool {
	return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
