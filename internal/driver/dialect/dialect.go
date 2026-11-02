// Package dialect 提供跨引擎的 SQL 方言处理：标识符引用、语句切分、
// 指纹归一化、语句分类与条件分析。
//
// 本包的函数必须是无状态纯函数，可被穷举单测。
package dialect

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/maintell/DBRover/internal/model"
)

// Class 是语句大类。
type Class uint8

const (
	ClassUnknown Class = iota
	ClassQuery
	ClassDML
	ClassDDL
	ClassDCL
	ClassTCL
)

func (c Class) String() string {
	switch c {
	case ClassQuery:
		return "query"
	case ClassDML:
		return "dml"
	case ClassDDL:
		return "ddl"
	case ClassDCL:
		return "dcl"
	case ClassTCL:
		return "tcl"
	default:
		return "unknown"
	}
}

// Statement 是切分后的一条语句。
type Statement struct {
	Text  string // 去首尾空白后的原始文本
	Start int    // 首个非空白字符的字节偏移
	End   int    // 语句结束偏移（不含分隔符）
	Line  int    // 起始行号（1 起）
	Index int    // 在脚本中的序号
}

// Dialect 是一个引擎的 SQL 方言。
type Dialect interface {
	Engine() model.Engine
	Syntax() Syntax

	QuoteIdent(name string) string
	QuoteQualified(parts ...string) string

	Split(script string) ([]Statement, error)
	Normalize(sqlText string) (string, error)
	Fingerprint(sqlText string) (string, error)
	Classify(sqlText string) (Class, error)
	Analyze(sqlText string) (Analysis, error)
}

type impl struct {
	engine model.Engine
	quote  func(string) string
}

// For 返回指定引擎的方言实现。未知引擎回落到 ANSI（双引号标识符）。
func For(e model.Engine) Dialect {
	switch e {
	case model.EnginePostgres:
		return impl{engine: e, quote: quoteDouble}
	case model.EngineMySQL, model.EngineMariaDB:
		return impl{engine: e, quote: quoteBacktick}
	case model.EngineMSSQL:
		return impl{engine: e, quote: quoteBracket}
	case model.EngineOracle:
		return impl{engine: e, quote: quoteDouble}
	case model.EngineSQLite:
		return impl{engine: e, quote: quoteDouble}
	default:
		return impl{engine: e, quote: quoteDouble}
	}
}

func (d impl) Engine() model.Engine { return d.engine }

func (d impl) Syntax() Syntax { return SyntaxFor(string(d.engine)) }

// QuoteIdent 引用单个标识符。所有由用户输入拼入 SQL 的标识符必须经过本方法。
func (d impl) QuoteIdent(name string) string { return d.quote(name) }

// QuoteQualified 引用多段限定名，如 QuoteQualified("db","t") -> `db`.`t`。
func (d impl) QuoteQualified(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, d.quote(p))
	}
	return strings.Join(out, ".")
}

func quoteDouble(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quoteBacktick(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func quoteBracket(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }

// Fingerprint 返回归一化 SQL 的 SHA-256 十六进制摘要。
func (d impl) Fingerprint(sqlText string) (string, error) {
	norm, err := d.Normalize(sqlText)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:]), nil
}

// FilterComments 去掉注释 Token。
func FilterComments(toks []Token) []Token {
	out := make([]Token, 0, len(toks))
	for _, t := range toks {
		if t.Kind != TokComment {
			out = append(out, t)
		}
	}
	return out
}
