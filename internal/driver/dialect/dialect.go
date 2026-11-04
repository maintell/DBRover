// Package dialect 提供跨引擎的 SQL 方言处理：标识符引用、语句切分、
// 指纹归一化、语句分类与条件分析。
//
// 本包的函数必须是无状态纯函数，可被穷举单测。
package dialect

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
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

	// SetNullabilitySQL 渲染「把某列改成可空 / 非空」所需的语句（可能不止一条）。
	//
	// 第二个返回 false 表示该引擎不支持原地改（SQLite 只能重建表）。MySQL 必须
	// 重写整个列定义（MODIFY COLUMN），所以要 typeName 与 defaultText——少了它们
	// 会顺手把原有的默认值丢掉，这是改类型那条路径上真机验证过的坑。
	SetNullabilitySQL(table, column, typeName, defaultText string, nullable bool) ([]string, bool)

	// SetDefaultSQL 渲染 SET / DROP DEFAULT。defaultText 为空表示 DROP DEFAULT。
	SetDefaultSQL(table, column, defaultText string) ([]string, bool)

	// AddCheckSQL 渲染加检查约束语句。expression 是已过白名单的表达式原文。
	//
	// 只在真正强制 CHECK 的引擎上调用：MySQL 5.7 会**解析但静默忽略** CHECK，
	// 建完看起来成功、约束根本不生效，比报错更糟。
	AddCheckSQL(table, name, expression string) string

	// DropCheckSQL 渲染删检查约束语句。MySQL 用 DROP CHECK，PostgreSQL 用
	// DROP CONSTRAINT —— 同一件事两个名字。
	DropCheckSQL(table, name string) string

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

	// ExplainSQL 渲染「只看计划、不执行」的语句；第二个返回 false 表示该引擎
	// 没法用一条语句表达执行计划（需要先开开关或查另一张表）。
	// 刻意不渲染 EXPLAIN ANALYZE：那会真的把语句跑一遍。
	ExplainSQL(sqlText string) (string, bool)

	// CreateSequenceSQL 渲染建序列语句；opts 里为零值的项不写子句（交给引擎默认）。
	CreateSequenceSQL(name string, opts SequenceOptions) string

	// DropSequenceSQL 渲染删序列语句。
	DropSequenceSQL(name string) string

	// CreateViewSQL 渲染建视图语句。name 应当是已引用好的限定名，
	// query 是已过校验的 SELECT / WITH 主体（不含分号）。
	CreateViewSQL(name, query string) string

	// CreateMaterializedViewSQL 渲染建物化视图语句（只有 PostgreSQL 有这个概念）。
	//
	// 与普通视图的区别不是语法糖：物化视图会把结果**存下来**，读得快但会过期，
	// 要 REFRESH 才更新——这句话得在界面上说清，否则用户会以为它跟视图一样实时。
	CreateMaterializedViewSQL(name, query string) string

	// DropMaterializedViewSQL 渲染删物化视图语句。
	DropMaterializedViewSQL(name string) string

	// DropViewSQL 渲染删视图语句。name 应当是已引用好的限定名。
	//
	// 视图是独立对象，不带 ON <table>。默认带 IF EXISTS：重复执行不该炸。
	// Oracle 没有 IF EXISTS 这个写法，只能裸 DROP VIEW——此时对象不存在就该报错。
	DropViewSQL(name string) string

	// DDLIncludesIndexes 表示这个引擎的「取建表语句」是否已经把索引包含在同一段
	// 文本里。
	//
	// MySQL 的 SHOW CREATE TABLE 自带、PostgreSQL 由驱动把 pg_get_indexdef 追加
	// 进去；SQLite 只存建表语句。导出脚本要据此决定是否补 CREATE INDEX，否则
	// 要么丢索引，要么撞名重复创建。
	DDLIncludesIndexes() bool

	// IndexBuildAdvice 返回「怎么建索引才不锁表」的建议；没有可说的时候返回空串。
	//
	// 这不是语句，是提示：PostgreSQL 的 CREATE INDEX CONCURRENTLY 不能在事务里跑，
	// 而本工具的执行门是单事务的——直接生成它会让每次执行都失败。
	// 所以这里只给出建议与限制，让人自己决定要不要去 SQL 工作区单跑。
	IndexBuildAdvice(table string, columns []string, unique bool) string

	Split(script string) ([]Statement, error)
	Normalize(sqlText string) (string, error)
	Fingerprint(sqlText string) (string, error)
	Classify(sqlText string) (Class, error)
	Analyze(sqlText string) (Analysis, error)

	// CrossDatabase 检测跨库引用（`库.模式.表`），返回引用原文。
	//
	// 只有 PostgreSQL 会命中：一个连接只对应一个库，三段名必然报
	// "cross-database references are not implemented"。其他引擎的
	// 三段名是正常写法（见 crossdb.go 的说明）。
	CrossDatabase(sqlText string) (string, bool)
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

// SetNullabilitySQL 见接口注释。
func (d impl) SetNullabilitySQL(table, column, typeName, defaultText string, nullable bool) ([]string, bool) {
	switch d.engine {
	case model.EngineSQLite:
		// SQLite 没有 ALTER COLUMN：改可空只能重建表，不在 v1 范围。
		return nil, false
	case model.EngineMySQL, model.EngineMariaDB:
		// MODIFY COLUMN 必须给完整定义，直接复用改类型那条路径（它已经处理了
		// 类型引用、默认值与 NOT NULL 的组合）。
		return d.AlterColumnTypeSQL(table, column, typeName, defaultText, nullable), true
	}
	verb := "SET NOT NULL"
	if nullable {
		verb = "DROP NOT NULL"
	}
	return []string{"ALTER TABLE " + table + " ALTER COLUMN " + d.QuoteIdent(column) + " " + verb}, true
}

// AddCheckSQL 见接口注释。
func (d impl) AddCheckSQL(table, name, expression string) string {
	return "ALTER TABLE " + table + " ADD CONSTRAINT " + d.QuoteIdent(name) + " CHECK (" + expression + ")"
}

// DropCheckSQL 见接口注释。
func (d impl) DropCheckSQL(table, name string) string {
	if d.engine == model.EngineMySQL || d.engine == model.EngineMariaDB {
		return "ALTER TABLE " + table + " DROP CHECK " + d.QuoteIdent(name)
	}
	return "ALTER TABLE " + table + " DROP CONSTRAINT " + d.QuoteIdent(name)
}

// SetDefaultSQL 见接口注释。
func (d impl) SetDefaultSQL(table, column, defaultText string) ([]string, bool) {
	if d.engine == model.EngineSQLite {
		return nil, false
	}
	col := d.QuoteIdent(column)
	if strings.TrimSpace(defaultText) == "" {
		return []string{"ALTER TABLE " + table + " ALTER COLUMN " + col + " DROP DEFAULT"}, true
	}
	return []string{"ALTER TABLE " + table + " ALTER COLUMN " + col + " SET DEFAULT " + defaultText}, true
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

// ExplainSQL 渲染执行计划语句。
//
// PostgreSQL 与 MySQL 都能直接 EXPLAIN 一条语句；SQLite 要用 EXPLAIN QUERY PLAN
// 才给出人可读的计划（裸 EXPLAIN 是字节码级跟踪，等于没用）。
// MSSQL 要先开 SET SHOWPLAN_ALL，Oracle 要 EXPLAIN PLAN FOR 再查 PLAN_TABLE——
// 这两者都不是「一条语句」，所以明确说不支持，而不是拼一条执行会失败的语句。
func (d impl) ExplainSQL(sqlText string) (string, bool) {
	switch d.engine {
	case model.EnginePostgres, model.EngineMySQL, model.EngineMariaDB:
		return "EXPLAIN " + sqlText, true
	case model.EngineSQLite:
		return "EXPLAIN QUERY PLAN " + sqlText, true
	default:
		return "", false
	}
}

// SequenceOptions 是建序列的可选项。零值字段表示「不指定，由引擎取默认」，
// 所以它们都是指针：`INCREMENT 0` 这种非法值要能被区分出来，而不是当成「没填」。
type SequenceOptions struct {
	Increment *int64
	Min       *int64
	Max       *int64
	Start     *int64
	Cycle     *bool
}

// CreateSequenceSQL 见接口注释。
//
// 只按 PostgreSQL 的写法生成：三种已实现引擎里只有 PG 支持序列（MySQL 用
// AUTO_INCREMENT、SQLite 用 AUTOINCREMENT），调用方应先看 Capabilities().Sequences。
// MSSQL / Oracle 的写法接近但未真机验证，接的时候再按 ADR-0005 方言化。
func (d impl) CreateSequenceSQL(name string, opts SequenceOptions) string {
	parts := []string{"CREATE SEQUENCE", name}
	if opts.Increment != nil {
		parts = append(parts, "INCREMENT BY", strconv.FormatInt(*opts.Increment, 10))
	}
	if opts.Min != nil {
		parts = append(parts, "MINVALUE", strconv.FormatInt(*opts.Min, 10))
	}
	if opts.Max != nil {
		parts = append(parts, "MAXVALUE", strconv.FormatInt(*opts.Max, 10))
	}
	if opts.Start != nil {
		parts = append(parts, "START WITH", strconv.FormatInt(*opts.Start, 10))
	}
	if opts.Cycle != nil {
		if *opts.Cycle {
			parts = append(parts, "CYCLE")
		} else {
			parts = append(parts, "NO CYCLE")
		}
	}
	return strings.Join(parts, " ")
}

// DropSequenceSQL 见接口注释。
func (d impl) DropSequenceSQL(name string) string {
	return "DROP SEQUENCE " + name
}

// CreateMaterializedViewSQL 见接口注释。
func (d impl) CreateMaterializedViewSQL(name, query string) string {
	return "CREATE MATERIALIZED VIEW " + name + " AS " + query
}

// DropMaterializedViewSQL 见接口注释。
func (d impl) DropMaterializedViewSQL(name string) string {
	return "DROP MATERIALIZED VIEW " + name
}

// CreateViewSQL 见接口注释。
//
// 不加 OR REPLACE：SQLite 不支持它，而「悄悄替换掉一个已有视图」也不是这里
// 该有的默认行为——同名视图存在时由服务层拒绝，让人自己确认。
func (d impl) CreateViewSQL(name, query string) string {
	return "CREATE VIEW " + name + " AS " + query
}

// DropViewSQL 渲染删视图语句。
func (d impl) DropViewSQL(name string) string {
	if d.engine == model.EngineOracle {
		return "DROP VIEW " + name
	}
	return "DROP VIEW IF EXISTS " + name
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

// CharLengthExpr 渲染「取该列字符数」的表达式。
//
// 同一个意思三个名字：MySQL / PostgreSQL / Oracle 用 CHAR_LENGTH，SQLite 只有
// LENGTH，SQL Server 用 LEN。放在方言层是因为这是纯语句文本差异（ADR-0005），
// service 层不该去判断引擎名。
//
// 注意 SQL Server 的 LEN 不计尾部空格，用它做「实际长度是否超限」的判据会偏
// 乐观。SQL Server 连接器还没做，等能真机验证时再改——宁可现在写清，也不要
// 让别人以为它和其余三个一样。
func CharLengthExpr(e model.Engine, quoted string) string {
	switch e {
	case model.EngineSQLite:
		return "LENGTH(" + quoted + ")"
	case model.EngineMSSQL:
		return "LEN(" + quoted + ")"
	}
	return "CHAR_LENGTH(" + quoted + ")"
}

// IndexBuildAdvice 按引擎给出「怎么建索引才不锁表」的建议。
//
// 只给建议、不自动生成 CONCURRENTLY：它不能在事务里跑，而本工具的执行门是
// 单事务的，直接发出去等于每次都失败。
// DDLIncludesIndexes 见接口注释：只有 SQLite 的建表语句不含独立索引。
//
// MySQL 的 SHOW CREATE TABLE 自带索引；PostgreSQL 由驱动在拼 DDL 时把
// pg_get_indexdef 追加进去（真机上验证过：再补一条会报 relation already exists）。
// 只有 SQLite 的 sqlite_master.sql 只存建表语句，索引是另一个对象。
func (d impl) DDLIncludesIndexes() bool {
	return d.engine != model.EngineSQLite
}

func (d impl) IndexBuildAdvice(table string, columns []string, unique bool) string {
	kw := "CREATE INDEX "
	if unique {
		kw = "CREATE UNIQUE INDEX "
	}
	cols := make([]string, len(columns))
	for i, c := range columns {
		cols[i] = d.quote(c)
	}
	plain := kw + d.quote("idx_name") + " ON " + table + " (" + strings.Join(cols, ", ") + ")"

	switch d.engine {
	case model.EnginePostgres:
		return "PostgreSQL 上大表建索引会持 ShareLock 挡住写入。要在线建请用 " +
			strings.Replace(plain, kw, "CREATE INDEX CONCURRENTLY ", 1) +
			"——注意 CONCURRENTLY 不能在事务里执行，本工具的执行门是单事务的，" +
			"需要它时请到 SQL 工作区单独跑。"
	case model.EngineMySQL:
		return "MySQL 5.7/8 的 InnoDB 加二级索引通常可以在线进行；写入压力大时可在语句末尾加 " +
			"ALGORITHM=INPLACE, LOCK=NONE 显式要求不锁写（不支持时会直接报错而不是退化成锁表）。"
	default:
		// SQLite 没有在线建索引的概念，建索引本来就很快。
		return ""
	}
}

// quoteBacktick 用反引号引用标识符（MySQL）。
func quoteBacktick(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
func quoteDouble(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

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
