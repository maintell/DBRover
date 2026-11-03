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

	// AlterColumnTypeSQL 返回「把某列改成新类型」所需的语句（可能不止一条）。
	//
	// defaultText 为空表示没有默认值；nullable 为 false 表示 NOT NULL。
	// 之所以要传完整定义：MySQL 的 MODIFY COLUMN 必须拿到完整列定义，
	// 否则会把原有的 NOT NULL 与默认值一起丢掉。
	// 返回空切片表示该引擎不支持改列类型（调用方应先看 Capabilities）。
	AlterColumnTypeSQL(table, column, typeName, defaultText string, nullable bool) []string

	// CreateIndexSQL 渲染建索引语句。columns 只给列名，引用由方言负责。
	CreateIndexSQL(table, name string, columns []string, unique bool) string

	// DropIndexSQL 渲染删索引语句。
	// MySQL 必须写 DROP INDEX <name> ON <table>，PostgreSQL / SQLite 不能带 ON。
	DropIndexSQL(table, name string) string

	// AddForeignKeySQL 渲染加外键语句。
	// MySQL 与 PostgreSQL 的写法一致；SQLite 不支持，调用方应先看 Capabilities。
	AddForeignKeySQL(table, name string, columns []string, refTable string, refColumns []string, onDelete, onUpdate string) string

	// DropForeignKeySQL 渲染删外键语句。
	// MySQL 用 DROP FOREIGN KEY，PostgreSQL 用 DROP CONSTRAINT——同一件事两个名字。
	DropForeignKeySQL(table, name string) string

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

// AlterColumnTypeSQL 渲染改列类型所需语句。
//
// PostgreSQL 用 ALTER COLUMN … TYPE，且 NOT NULL 与默认值要单独设置；
// MySQL 用 MODIFY COLUMN，一条语句里必须给完整列定义（否则会丢掉 NOT NULL 与默认值）；
// SQLite 不支持原地改类型（只能重建表），返回空切片。
func (d impl) AlterColumnTypeSQL(table, column, typeName, defaultText string, nullable bool) []string {
	col := d.quote(column)
	switch d.engine {
	case model.EnginePostgres:
		out := []string{"ALTER TABLE " + table + " ALTER COLUMN " + col + " TYPE " + typeName}
		if !nullable {
			out = append(out, "ALTER TABLE "+table+" ALTER COLUMN "+col+" SET NOT NULL")
		}
		if defaultText != "" {
			out = append(out, "ALTER TABLE "+table+" ALTER COLUMN "+col+" SET DEFAULT "+defaultText)
		}
		return out
	case model.EngineMySQL:
		def := col + " " + typeName
		if !nullable {
			def += " NOT NULL"
		}
		if defaultText != "" {
			def += " DEFAULT " + defaultText
		}
		return []string{"ALTER TABLE " + table + " MODIFY COLUMN " + def}
	default:
		// SQLite / 未支持引擎：改列类型需要重建表，不在本方法里假装能做。
		return nil
	}
}

// CreateIndexSQL 渲染建索引语句。
// 三种关系引擎的写法一致：CREATE [UNIQUE] INDEX name ON table (cols)。
func (d impl) CreateIndexSQL(table, name string, columns []string, unique bool) string {
	cols := make([]string, 0, len(columns))
	for _, c := range columns {
		cols = append(cols, d.quote(c))
	}
	kw := "CREATE INDEX "
	if unique {
		kw = "CREATE UNIQUE INDEX "
	}
	return kw + d.quote(name) + " ON " + table + " (" + strings.Join(cols, ", ") + ")"
}

// DropIndexSQL 渲染删索引语句。
//
// 只有 MySQL 要带 ON <table>（它把索引当成表的附属对象）；
// PostgreSQL 与 SQLite 的索引是独立对象，带上 ON 反而是语法错误。
func (d impl) DropIndexSQL(table, name string) string {
	switch d.engine {
	case model.EngineMySQL, model.EngineMSSQL:
		return "DROP INDEX " + d.quote(name) + " ON " + table
	default:
		return "DROP INDEX " + d.quote(name)
	}
}

// AddForeignKeySQL 渲染加外键语句。
//
// MySQL 与 PostgreSQL 的写法一致。动作子句（ON DELETE / ON UPDATE）只在
// 调用方给了合法值时出现；合法性由 service 的白名单保证，这里不做判断。
func (d impl) AddForeignKeySQL(table, name string, columns []string, refTable string,
	refColumns []string, onDelete, onUpdate string) string {

	cols := make([]string, len(columns))
	for i, c := range columns {
		cols[i] = d.quote(c)
	}
	refs := make([]string, len(refColumns))
	for i, c := range refColumns {
		refs[i] = d.quote(c)
	}

	sql := "ALTER TABLE " + table + " ADD CONSTRAINT " + d.quote(name) +
		" FOREIGN KEY (" + strings.Join(cols, ", ") + ")" +
		" REFERENCES " + refTable + " (" + strings.Join(refs, ", ") + ")"
	if onDelete != "" {
		sql += " ON DELETE " + onDelete
	}
	if onUpdate != "" {
		sql += " ON UPDATE " + onUpdate
	}
	return sql
}

// DropForeignKeySQL 渲染删外键语句。
//
// 同一件事两个名字：MySQL 在 ALTER TABLE 后面说 DROP FOREIGN KEY，
// PostgreSQL 说 DROP CONSTRAINT（它不区分约束类型）。
func (d impl) DropForeignKeySQL(table, name string) string {
	switch d.engine {
	case model.EngineMySQL, model.EngineMSSQL:
		return "ALTER TABLE " + table + " DROP FOREIGN KEY " + d.quote(name)
	default:
		return "ALTER TABLE " + table + " DROP CONSTRAINT " + d.quote(name)
	}
}

// quoteBacktick 用反引号引用标识符（MySQL）。
func quoteBacktick(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func quoteDouble(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

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
