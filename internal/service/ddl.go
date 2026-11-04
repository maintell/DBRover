package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/dialect"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
)

// ErrSchemaChanged 表示表定义在预案生成后发生了变化。
// 结构和数据不一样：拿旧结构去改已被别人改过的表，生成的语句可能直接不合法。
var ErrSchemaChanged = errors.New("表定义已变化，请重新生成结构变更预案")

// ErrInvalidDDL 表示结构变更参数不合法（列名 / 类型 / 默认值）。
var ErrInvalidDDL = errors.New("结构变更参数不合法")

// ErrCheckViolated 表示存量行不满足这条检查约束。
var ErrCheckViolated = errors.New("存量行不满足检查约束")

// ErrNullableBlocked 表示列里还有 NULL，不能改成 NOT NULL。
//
// 单独一个错误码，是因为「引擎拒绝」与「我们自己先看见了」不是一回事：
// 前者给不出「多少行」，用户只能自己写 SQL 去数。
var ErrNullableBlocked = errors.New("列里还有 NULL，不能改成非空")

// DDLAction 是结构变更动作。
//
// v1 只做加列与删列：改列类型需要「收窄预检」（先查越界数据）与跨引擎重写语义，
// 建表需要完整的列集合校验，两者都不在这里硬做半成品（见 docs/07 §7 状态表）。
type DDLAction string

const (
	DDLAddColumn       DDLAction = "add_column"
	DDLDropColumn      DDLAction = "drop_column"
	DDLCreateTable     DDLAction = "create_table"
	DDLAlterColumnType DDLAction = "alter_column_type"
	DDLCreateIndex     DDLAction = "create_index"
	DDLDropIndex       DDLAction = "drop_index"
	DDLAddForeignKey   DDLAction = "add_foreign_key"
	DDLDropTable       DDLAction = "drop_table"
	// DDLSetNullable 改一列的可空性（SET / DROP NOT NULL）。
	DDLSetNullable DDLAction = "set_nullable"
	// DDLSetDefault 改一列的默认值（SET / DROP DEFAULT）。只影响之后的写入。
	DDLSetDefault DDLAction = "set_default"
	// DDLAddCheck 加一条检查约束。加之前先数一遍不满足表达式的行。
	DDLAddCheck DDLAction = "add_check"
	// DDLCreateView 建一个视图。视图只存查询定义，不复制数据。
	DDLCreateView DDLAction = "create_view"
	// DDLRaw 是「我已经有 DDL，只想让它经过这道门」：语句由用户写，
	// 工具不做语义解析，只负责分级、审批、审计与竞态保护。
	DDLRaw DDLAction = "raw"
)

// ErrForeignKeyBlocked 表示既有数据不满足要加的外键（有孤儿行，或父表列不唯一）。
var ErrForeignKeyBlocked = errors.New("既有数据不满足该外键，加约束会失败")

// ErrDependencyBlocked 表示目标对象仍被别的对象引用，删除会连带打断它们。
//
// 只在生成预案时阻断：错误信息里必须列出「谁在引用」，否则用户无从下手。
var ErrDependencyBlocked = errors.New("目标仍被其他对象引用，删除会打断它们")

// ForeignKeySpec 是加外键的描述。Name 为空时按表名与列名生成一个确定的名字。
type ForeignKeySpec struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	RefTable   string   `json:"ref_table"`
	RefColumns []string `json:"ref_columns"`
	// OnDelete / OnUpdate 只接受白名单里的动作（见 fkActions）。
	OnDelete string `json:"on_delete,omitempty"`
	OnUpdate string `json:"on_update,omitempty"`
}

// safeDefaultText 把元数据里的默认值转成可拼进语句的文本。
//
// 之所以要看列类型：MySQL 的 information_schema 对字符串默认值给出的是不带引号的
// 裸值（`d` 而不是 `'d'`），直接拼进 MODIFY COLUMN 就是语法错误。
// PostgreSQL 给的是 `'d'::character varying`，已经可直接使用。
func safeDefaultText(kind model.TypeKind, def *string) string {
	if def == nil {
		return ""
	}
	d := strings.TrimSpace(*def)
	if d == "" || strings.ContainsAny(d, ";") {
		return ""
	}
	if defaultNeedsQuoting(kind, d) {
		return "'" + strings.ReplaceAll(d, "'", "''") + "'"
	}
	return d
}

// defaultNeedsQuoting 判断一个默认值文本是不是「未加引号的裸字符串」。
func defaultNeedsQuoting(kind model.TypeKind, d string) bool {
	if strings.HasPrefix(d, "'") || strings.HasPrefix(d, "-") {
		return false
	}
	if d[0] >= '0' && d[0] <= '9' {
		return false
	}
	switch strings.ToUpper(d) {
	case "NULL", "TRUE", "FALSE", "CURRENT_TIMESTAMP", "CURRENT_DATE",
		"CURRENT_TIME", "CURRENT_USER", "NOW()", "UUID()":
		return false
	}
	// 带括号的视为函数调用/表达式，原样保留。
	if strings.ContainsAny(d, "()") {
		return false
	}
	switch kind {
	case model.TypeString, model.TypeText, model.TypeJSON, model.TypeUUID,
		model.TypeEnum, model.TypeGeometry, model.TypeNetwork:
		return true
	}
	return false
}

// ColumnSpec 是加列时的列定义。
type ColumnSpec struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default,omitempty"`
}

// DDLRequest 是一次结构变更请求。
type DDLRequest struct {
	Table  model.ObjectRef `json:"table"`
	Action DDLAction       `json:"action"`
	// Column 用于 add_column / drop_column。
	Column ColumnSpec `json:"column"`
	// Columns 与 PrimaryKey 只在 create_table 时有意义。
	// v1 要求必须给主键：没有主键的新表在本工具里不可编辑（浏览要键集分页），
	// 建出来只会让人困惑。
	Columns    []ColumnSpec `json:"columns"`
	PrimaryKey []string     `json:"primary_key"`
	// Index 用于 create_index / drop_index（drop 时只用到 Name）。
	Index IndexSpec `json:"index"`
	// ForeignKey 用于 add_foreign_key。
	ForeignKey ForeignKeySpec `json:"foreign_key"`
	// Check 用于 add_check。名字必须显式给：匿名约束在 PG 里会由引擎起名，
	// 回滚时就找不回它是哪一条了。
	Check CheckSpec `json:"check"`
	// View 用于 create_view。
	View ViewSpec `json:"view"`
	// RawSQL 用于 raw：一条手写的 DDL 语句。
	RawSQL string `json:"sql"`
}

// CheckSpec 是一条检查约束。
type CheckSpec struct {
	Name string `json:"name"`
	// Expression 是布尔表达式原文，例如 age >= 0 AND age < 200。
	//
	// 它会被拼进 DDL，所以只允许「不像注入」的字符：见 normalizeCheckExpression。
	Expression string `json:"expression"`
}

// ViewSpec 是一个视图定义。
type ViewSpec struct {
	Name string `json:"name"`
	// Query 是 SELECT / WITH 主体（不含 CREATE VIEW 与分号）。
	Query string `json:"query"`
}

// constraintNamePattern 限定约束名：字母或下划线开头，其后字母数字下划线。
//
// 比列名更严：约束名要出现在 ALTER TABLE ... ADD CONSTRAINT 与回滚语句里，
// 宽松一点就得靠 QuoteIdent 兜住各种怪字符，而约束名本来就没有必要长成那样。
var constraintNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// typePattern 限定类型声明的字符集。
//
// 类型名无法参数化，只能靠白名单挡住分号与注释符 —— 这是「拼接只允许发生在
// 标识符引用场景」之外唯一的例外，因此必须比 QuoteIdent 更严格。
// 允许：integer / character varying(64) / numeric(10, 2) / timestamp with time zone
var typePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_ ]{0,40}(\s*\(\s*[0-9]{1,6}(\s*,\s*[0-9]{1,6})?\s*\))?$`)

// defaultPattern 限定默认值字面量：NULL / 布尔 / 数字 / 单引号字符串 / 当前时间。
// 表达式型默认值（如 nextval(...)）一律拒绝：它是一条任意 SQL，不能当字面量处理。
var defaultPattern = regexp.MustCompile(`(?i)^(NULL|TRUE|FALSE|-?[0-9]{1,20}(\.[0-9]{1,10})?|'(?:[^']|''){0,200}'|CURRENT_TIMESTAMP|CURRENT_DATE|CURRENT_TIME|now\(\))$`)

// ddlHash 为表定义算一个短哈希，作为「我看的是这一版结构」的前置条件。
func ddlHash(ddlText string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(ddlText)))
	return hex.EncodeToString(sum[:8])
}

// DDLHash 是 ddlHash 的对外形式：API 层为撤销预案设置前置条件时要用。
func DDLHash(ddlText string) string { return ddlHash(ddlText) }

// TableDDL 返回表定义原文。结构页展示与哈希校验共用它，避免两处各写一次。
func (e *Executor) TableDDL(ctx context.Context, ref model.ObjectRef) (string, error) {
	return e.conn.Meta().DDL(ctx, ref)
}

// DescribeTable 返回表结构详情（列、索引、外键、检查约束）。
func (e *Executor) DescribeTable(ctx context.Context, ref model.ObjectRef) (*model.TableDetail, error) {
	return e.conn.Meta().DescribeTable(ctx, ref)
}

func findColumn(td *model.TableDetail, name string) (model.Column, bool) {
	for _, c := range td.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return model.Column{}, false
}

// normalizeColumnName 校验列名。加列与删列都要过这一关。
func normalizeColumnName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%w: 列名不能为空", ErrInvalidDDL)
	}
	if len(name) > 128 {
		return "", fmt.Errorf("%w: 列名过长", ErrInvalidDDL)
	}
	// 控制字符与引号一律拒绝：引号会改变标识符边界，控制字符没有正当用途。
	for _, r := range name {
		if r < 0x20 || r == '"' || r == '`' || r == '\'' || r == ';' || r == '\\' {
			return "", fmt.Errorf("%w: 列名含非法字符", ErrInvalidDDL)
		}
	}
	return name, nil
}

// normalizeColumn 校验列定义。返回的 Type/Default 已去掉首尾空白。
func normalizeColumn(spec ColumnSpec) (ColumnSpec, error) {
	name, err := normalizeColumnName(spec.Name)
	if err != nil {
		return spec, err
	}

	typ := strings.TrimSpace(spec.Type)
	if !typePattern.MatchString(typ) {
		return spec, fmt.Errorf("%w: 类型 %q 不在允许的形态内", ErrInvalidDDL, spec.Type)
	}

	out := ColumnSpec{Name: name, Type: typ, Nullable: spec.Nullable}
	if spec.Default != nil {
		d := strings.TrimSpace(*spec.Default)
		if !defaultPattern.MatchString(d) {
			return spec, fmt.Errorf("%w: 默认值 %q 只允许字面量", ErrInvalidDDL, *spec.Default)
		}
		out.Default = &d
	}
	return out, nil
}

// tableExists 判断表是否存在。
// 用对象清单而不是「DescribeTable 是否报错」：后者分不清「不存在」与「读失败」，
// 把读失败当成不存在去建表，就会撞上引擎的 already exists。
func (e *Executor) tableExists(ctx context.Context, ref model.ObjectRef) (bool, error) {
	objs, err := e.conn.Meta().Objects(ctx, ref.Catalog, schemaOf(ref, e.cfgSchema()), model.KindTable)
	if err != nil {
		return false, err
	}
	for _, o := range objs {
		if strings.EqualFold(o.Name, ref.Name) {
			return true, nil
		}
	}
	return false, nil
}

// columnDefinition 由列定义拼出 DDL 片段。
func (e *Executor) columnDefinition(spec ColumnSpec) string {
	parts := []string{e.dialect.QuoteIdent(spec.Name), spec.Type}
	if !spec.Nullable {
		parts = append(parts, "NOT NULL")
	}
	if spec.Default != nil {
		parts = append(parts, "DEFAULT", *spec.Default)
	}
	return strings.Join(parts, " ")
}

// columnDefinitionFromColumn 用引擎报告的列定义拼回 DDL 片段（删列的回滚用）。
// 默认值来自元数据而不是用户输入，但仍做一次形态检查：宁可不还原默认值，
// 也不把一段无法验证的文本拼进语句。
func (e *Executor) columnDefinitionFromColumn(c model.Column) string {
	typ := strings.TrimSpace(c.Type.Raw)
	if typ == "" || !typePattern.MatchString(typ) {
		typ = "TEXT"
	}
	parts := []string{e.dialect.QuoteIdent(c.Name), typ}
	if !c.Nullable {
		parts = append(parts, "NOT NULL")
	}
	if d := safeDefaultText(c.Type.Kind, c.Default); d != "" {
		parts = append(parts, "DEFAULT", d)
	}
	return strings.Join(parts, " ")
}

// countWhere 数出满足条件的行数；查不出来返回 -1（未知）。
//
// 「未知」必须与 0 分开：删列、改非空都靠这个数告诉用户影响面，
// 拿 0 假装「没有数据」会让最危险的操作看起来最安全。
func (e *Executor) countWhere(ctx context.Context, table, where string) int64 {
	n, err := e.countWhereErr(ctx, table, where)
	if err != nil {
		return -1
	}
	return n
}

// countWhereErr 同上，但把「查不出来」的原因带回去。
//
// 加检查约束时需要区分「0 行不满足」和「根本查不出来」：前者可以放行，
// 后者只能交给引擎并在预案里说明——把两者混成 -1 会让人以为已经预检过。
func (e *Executor) countWhereErr(ctx context.Context, table, where string) (int64, error) {
	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = sess.Close() }()

	cur, err := sess.Query(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+where, nil, driver.QueryOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = cur.Close() }()

	rows, _, err := cur.Next(ctx)
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || len(rows[0]) == 0 {
		return 0, fmt.Errorf("COUNT 返回了意外形状")
	}
	n, ok := toInt64(rows[0][0])
	if !ok {
		return 0, fmt.Errorf("COUNT 结果不是整数")
	}
	return n, nil
}

// countNonNull 数出某列的非空行数，用于告知「删这一列会丢多少数据」。
func (e *Executor) countNonNull(ctx context.Context, table, column string) int64 {
	return e.countWhere(ctx, table, e.dialect.QuoteIdent(column)+" IS NOT NULL")
}

// countNulls 数出某列的空行数，用于「能不能改成 NOT NULL」的预检。
func (e *Executor) countNulls(ctx context.Context, table, column string) int64 {
	return e.countWhere(ctx, table, e.dialect.QuoteIdent(column)+" IS NULL")
}

// checkUnsupportedReason 给出「为什么这个引擎不能加检查约束」的一句话。
//
// 写清楚是因为这条最容易被误解成我们的偷懒：MySQL 5.7 接受 CHECK 语法、
// SHOW CREATE TABLE 里也能看到它，但**插入违反约束的行照样成功**。
func checkUnsupportedReason(eng model.Engine) string {
	switch eng {
	case model.EngineMySQL, model.EngineMariaDB:
		return "MySQL 5.7 会解析 CHECK 但静默忽略，8.0.16 起才真正强制"
	case model.EngineSQLite:
		return "SQLite 只能在建表时写 CHECK，ALTER TABLE 加不上"
	}
	return "该引擎没有声明支持检查约束"
}

// normalizeCheckExpression 校验并整理一条检查约束表达式。
//
// 表达式是要拼进 DDL 的 SQL 片段，这是我们唯一无法参数化的地方（约束不能带
// 占位符），所以只能按「不像注入」来卡：
//   - 分号：会把一条语句变成两条，执行门会切成两条并在第二段失败，报错完全
//     指不到这里；
//   - 行注释与块注释：能把后面的内容整段吃掉，包括我们补上的右括号；
//   - 控制字符：没有正当用途，只会让错误信息变成乱码。
//
// 不做的：不去解析表达式语法。约束的合法性最终由引擎判定，我们只保证它不会
// 「跑出本条语句之外」。
func normalizeCheckExpression(expr string) (string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", fmt.Errorf("%w: 检查表达式不能为空", ErrInvalidDDL)
	}
	if len(expr) > 1000 {
		return "", fmt.Errorf("%w: 检查表达式过长（>1000 字符）", ErrInvalidDDL)
	}
	if strings.ContainsAny(expr, ";") {
		return "", fmt.Errorf("%w: 检查表达式里不能有分号（会把一条语句变成两条）", ErrInvalidDDL)
	}
	if strings.Contains(expr, "--") || strings.Contains(expr, "/*") || strings.Contains(expr, "*/") {
		return "", fmt.Errorf("%w: 检查表达式里不能有注释符", ErrInvalidDDL)
	}
	for _, r := range expr {
		if r < 0x20 {
			return "", fmt.Errorf("%w: 检查表达式里有控制字符", ErrInvalidDDL)
		}
	}
	return expr, nil
}

// BuildDDLPlan 生成一次结构变更的预案。
//
// 与数据变更共用同一套语义：先给语句、回滚、风险与摩擦，确认后才由 ApplyChangePlan 执行。
// 两点不同：
//   - DDL 无法参数化，列名走 QuoteIdent、类型与默认值走白名单；
//   - 预案带一个「表定义哈希」，执行前比对，防止拿旧结构去改别人已经改过的表。
func (e *Executor) BuildDDLPlan(ctx context.Context, req DDLRequest) (*ChangePlan, error) {
	// 建表没有「现有结构」可读，走单独分支。
	if req.Action == DDLCreateTable {
		return e.buildCreateTablePlan(ctx, req)
	}
	// 建视图同理：它没有「现有表结构」可读，也不需要表定义哈希。
	if req.Action == DDLCreateView {
		return e.buildCreateViewPlan(ctx, req)
	}
	// 手写 DDL 也不依赖「我要改的那张表」：它可能建表、也可能改别的东西。
	if req.Action == DDLRaw {
		return e.buildRawDDLPlan(ctx, req)
	}

	td, err := e.conn.Meta().DescribeTable(ctx, req.Table)
	if err != nil {
		return nil, err
	}
	currentDDL, err := e.conn.Meta().DDL(ctx, req.Table)
	if err != nil {
		return nil, err
	}

	// 删列与索引操作只需要列名/索引名；加列与改类型才需要完整列定义。
	var spec ColumnSpec
	switch req.Action {
	case DDLDropColumn:
		name, nerr := normalizeColumnName(req.Column.Name)
		if nerr != nil {
			return nil, nerr
		}
		spec = ColumnSpec{Name: name}
	case DDLAddColumn, DDLAlterColumnType:
		var cerr error
		spec, cerr = normalizeColumn(req.Column)
		if cerr != nil {
			return nil, cerr
		}
	case DDLAddCheck:
		// 约束名与表达式在这里就校验，别等到拼语句时才发现。
		name := strings.TrimSpace(req.Check.Name)
		if !constraintNamePattern.MatchString(name) {
			return nil, fmt.Errorf("%w: 约束名 %q 只能用字母、数字、下划线，且以字母或下划线开头（回滚要用它）",
				ErrInvalidDDL, req.Check.Name)
		}
		spec = ColumnSpec{Name: name}
	case DDLSetNullable, DDLSetDefault:
		// 这两个动作只动列的一两个属性，不需要（也不该要求）重报类型。
		name, nerr := normalizeColumnName(req.Column.Name)
		if nerr != nil {
			return nil, nerr
		}
		spec = ColumnSpec{Name: name, Default: req.Column.Default, Nullable: req.Column.Nullable}
	}

	table := e.dialect.QuoteQualified(schemaOf(req.Table, e.cfgSchema()), req.Table.Name)
	plan := &ChangePlan{
		ID:            "ddl-" + newID(),
		Table:         req.Table,
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(planTTL),
		DDL:           true,
		ExpectDDLHash: ddlHash(currentDDL),
	}

	switch req.Action {
	case DDLAddColumn:
		if existing, ok := findColumn(td, spec.Name); ok {
			return nil, fmt.Errorf("%w: 列 %q 已存在", ErrInvalidDDL, existing.Name)
		}
		plan.Statements = []PlanStatement{{
			SQL: "ALTER TABLE " + table + " ADD COLUMN " + e.columnDefinition(spec),
			// DDL 的影响行数由引擎决定（多数是 0），不用行数校验。
			ExpectRows: -1,
		}}
		plan.Rollback = []PlanStatement{{
			SQL:        "ALTER TABLE " + table + " DROP COLUMN " + e.dialect.QuoteIdent(spec.Name),
			ExpectRows: -1,
		}}
		plan.AffectedRows = 0

	case DDLDropColumn:
		col, ok := findColumn(td, req.Column.Name)
		if !ok {
			return nil, fmt.Errorf("%w: 列 %q 不存在", ErrInvalidDDL, req.Column.Name)
		}
		if col.IsPrimary {
			return nil, fmt.Errorf("%w: 主键列不能删除", ErrInvalidDDL)
		}
		// 引用这一列的外键必须先拆：引擎会拒绝，而报错里不会说清是谁在引用。
		if err := e.blockColumnDependents(ctx, req.Table, col.Name); err != nil {
			return nil, err
		}
		// 视图依赖只能按表级文本匹配发现，无法判断是否引用了**这一列**
		// （视图体里可能只是 SELECT *）。这条不阻断，但必须让用户知道。
		if deps := e.viewDependents(ctx, req.Table); len(deps) > 0 {
			plan.Notes = append(plan.Notes, fmt.Sprintf(
				"该表还有 %d 个视图依赖（%s），删列前请自查视图是否用到 %q",
				len(deps), strings.Join(deps, "、"), col.Name))
		}
		plan.Statements = []PlanStatement{{
			SQL:        "ALTER TABLE " + table + " DROP COLUMN " + e.dialect.QuoteIdent(col.Name),
			ExpectRows: -1,
		}}
		// 回滚只能把列加回去，里面的数据回不来：这件事必须在预案里说清，
		// 不能给一个「看起来能撤销」的按钮。
		plan.Rollback = []PlanStatement{{
			SQL:        "ALTER TABLE " + table + " ADD COLUMN " + e.columnDefinitionFromColumn(col),
			ExpectRows: -1,
		}}
		plan.Irreversible = true
		plan.AffectedRows = e.countNonNull(ctx, table, col.Name)

	case DDLAlterColumnType:
		col, ok := findColumn(td, req.Column.Name)
		if !ok {
			return nil, fmt.Errorf("%w: 列 %q 不存在", ErrInvalidDDL, req.Column.Name)
		}
		if !e.conn.Capabilities().AlterColumnType {
			return nil, fmt.Errorf("%w: 该引擎不支持原地修改列类型（SQLite 只能重建表）", ErrInvalidDDL)
		}
		newType := strings.TrimSpace(req.Column.Type)
		if !typePattern.MatchString(newType) {
			return nil, fmt.Errorf("%w: 类型 %q 不在允许的形态内", ErrInvalidDDL, req.Column.Type)
		}
		oldType := strings.TrimSpace(col.Type.Raw)
		if oldType == "" {
			return nil, fmt.Errorf("%w: 读不到列 %q 的当前类型，无法生成回滚语句", ErrInvalidDDL, col.Name)
		}

		// 收窄预检：新类型有容量上限时，先数一遍装不下的行。
		// 探测失败按「未知」放行，让引擎自己报错；探测成功且有越界数据则拒绝。
		bad, samples, why, perr := e.probeNarrowing(ctx, table, e.dialect.QuoteIdent(col.Name), newType)
		if perr == nil && bad > 0 {
			return nil, fmt.Errorf("%w：%d 行%s，例如 %s",
				ErrNarrowingBlocked, bad, why, strings.Join(samples, "、"))
		}

		def := safeDefaultText(col.Type.Kind, col.Default)
		fwd := e.dialect.AlterColumnTypeSQL(table, col.Name, newType, def, col.Nullable)
		if len(fwd) == 0 {
			return nil, fmt.Errorf("%w: 无法为 %s 渲染改列类型语句", ErrInvalidDDL, e.dialect.Engine())
		}
		back := e.dialect.AlterColumnTypeSQL(table, col.Name, oldType, def, col.Nullable)
		for _, s := range fwd {
			plan.Statements = append(plan.Statements, PlanStatement{SQL: s, ExpectRows: -1})
		}
		for _, s := range back {
			plan.Rollback = append(plan.Rollback, PlanStatement{SQL: s, ExpectRows: -1})
		}
		if td.RowEstimate > 0 {
			plan.AffectedRows = td.RowEstimate
		} else {
			plan.AffectedRows = -1 // 未知就说未知，不写 0
		}

	case DDLSetNullable:
		col, ok := findColumn(td, spec.Name)
		if !ok {
			return nil, fmt.Errorf("%w: 列 %q 不存在", ErrInvalidDDL, spec.Name)
		}
		if !e.conn.Capabilities().AlterColumnNullability {
			return nil, fmt.Errorf("%w: 该引擎不支持原地改可空性（SQLite 只能重建表）", ErrInvalidDDL)
		}
		if col.Nullable == spec.Nullable {
			state := "可空"
			if !spec.Nullable {
				state = "非空"
			}
			return nil, fmt.Errorf("%w: 列 %q 已经是%s，这次改动什么都不会改变", ErrInvalidDDL, col.Name, state)
		}
		// 改成 NOT NULL 前先数一遍 NULL：引擎也会拒绝，但它的报错里没有「多少行」，
		// 用户只能自己再写一条 SQL 去数。探测失败（-1）就交给引擎判。
		if !spec.Nullable {
			if n := e.countNulls(ctx, table, col.Name); n > 0 {
				return nil, fmt.Errorf("%w：列 %q 有 %d 行为 NULL，先补上值再改非空", ErrNullableBlocked, col.Name, n)
			}
		}
		def := safeDefaultText(col.Type.Kind, col.Default)
		rawType := strings.TrimSpace(col.Type.Raw)
		fwd, okCap := e.dialect.SetNullabilitySQL(table, col.Name, rawType, def, spec.Nullable)
		if !okCap || len(fwd) == 0 {
			return nil, fmt.Errorf("%w: 无法为 %s 渲染改可空语句", ErrInvalidDDL, e.dialect.Engine())
		}
		back, _ := e.dialect.SetNullabilitySQL(table, col.Name, rawType, def, col.Nullable)
		for _, s := range fwd {
			plan.Statements = append(plan.Statements, PlanStatement{SQL: s, ExpectRows: -1})
		}
		for _, s := range back {
			plan.Rollback = append(plan.Rollback, PlanStatement{SQL: s, ExpectRows: -1})
		}
		// 改可空不改任何一行的值：不写 0 是因为 PG 的 SET NOT NULL 会全表校验，
		// 说「0 行」会让人以为瞬间完成。
		plan.AffectedRows = -1
		if !spec.Nullable {
			plan.Notes = append(plan.Notes, "改非空会全表校验一遍：大表上可能锁住写入较久")
		}

	case DDLSetDefault:
		col, ok := findColumn(td, spec.Name)
		if !ok {
			return nil, fmt.Errorf("%w: 列 %q 不存在", ErrInvalidDDL, spec.Name)
		}
		if !e.conn.Capabilities().AlterColumnDefault {
			return nil, fmt.Errorf("%w: 该引擎不支持原地改默认值（SQLite 只能重建表）", ErrInvalidDDL)
		}
		want := ""
		if spec.Default != nil {
			want = strings.TrimSpace(*spec.Default)
		}
		if want != "" && !defaultPattern.MatchString(want) {
			return nil, fmt.Errorf("%w: 默认值 %q 不是字面量；表达式一律拒绝（它是一条任意 SQL）",
				ErrInvalidDDL, want)
		}
		cur := safeDefaultText(col.Type.Kind, col.Default)
		// 当前默认值是表达式时 safeDefaultText 会给出空串；此时回滚只能变成
		// DROP DEFAULT——那是「改结构却给不出等价回滚」，必须当场说清楚。
		if col.Default != nil && strings.TrimSpace(*col.Default) != "" && cur == "" {
			return nil, fmt.Errorf("%w: 列 %q 的当前默认值是表达式（%s），生成不出等价回滚语句",
				ErrInvalidDDL, col.Name, strings.TrimSpace(*col.Default))
		}
		if cur == want {
			return nil, fmt.Errorf("%w: 列 %q 的默认值本来就是 %q", ErrInvalidDDL, col.Name, want)
		}
		fwd, okCap := e.dialect.SetDefaultSQL(table, col.Name, want)
		if !okCap || len(fwd) == 0 {
			return nil, fmt.Errorf("%w: 无法为 %s 渲染改默认值语句", ErrInvalidDDL, e.dialect.Engine())
		}
		back, _ := e.dialect.SetDefaultSQL(table, col.Name, cur)
		for _, s := range fwd {
			plan.Statements = append(plan.Statements, PlanStatement{SQL: s, ExpectRows: -1})
		}
		for _, s := range back {
			plan.Rollback = append(plan.Rollback, PlanStatement{SQL: s, ExpectRows: -1})
		}
		plan.AffectedRows = 0
		plan.Notes = append(plan.Notes, "改默认值只影响之后的写入，不会回填已有行")

	case DDLAddCheck:
		// 先校验表达式再看引擎能力：参数本身有错时，这句话在哪台引擎上都不成立，
		// 说「该引擎不支持」会把人的注意力引到错的方向。
		expr, xerr := normalizeCheckExpression(req.Check.Expression)
		if xerr != nil {
			return nil, xerr
		}
		if !e.conn.Capabilities().AddCheck {
			return nil, fmt.Errorf("%w: %s 加检查约束不可靠——%s",
				ErrInvalidDDL, e.conn.Engine(), checkUnsupportedReason(e.conn.Engine()))
		}
		for _, c := range td.Checks {
			if strings.EqualFold(c.Name, spec.Name) {
				return nil, fmt.Errorf("%w: 约束 %q 已存在", ErrInvalidDDL, c.Name)
			}
		}
		// 加约束不改数据，但引擎会全表校验；先把「有多少行过不了」数出来。
		// 否则用户拿到的是引擎的一句 "check constraint is violated by some row"，
		// 既不知道多少行、也不知道是哪几行。
		bad, cerr := e.countWhereErr(ctx, table, "NOT ("+expr+")")
		if cerr == nil && bad > 0 {
			return nil, fmt.Errorf("%w：有 %d 行不满足 %s，先修数据再加约束",
				ErrCheckViolated, bad, expr)
		}
		if cerr != nil {
			// 查不出来就说查不出来：让用户以为预检过了，等于把问题推迟到引擎报错。
			plan.Notes = append(plan.Notes, "无法预检存量行（"+cerr.Error()+"），交由引擎校验")
		}
		fwd := e.dialect.AddCheckSQL(table, spec.Name, expr)
		back := e.dialect.DropCheckSQL(table, spec.Name)
		plan.Statements = []PlanStatement{{SQL: fwd, ExpectRows: -1}}
		plan.Rollback = []PlanStatement{{SQL: back, ExpectRows: -1}}
		plan.AffectedRows = 0
		plan.Notes = append(plan.Notes, "加约束不改数据，但引擎会全表校验一遍：大表上可能锁住写入较久")

	case DDLCreateIndex:
		if !e.conn.Capabilities().Indexes {
			return nil, fmt.Errorf("%w: 该引擎不支持索引管理", ErrInvalidDDL)
		}
		spec, nerr := normalizeIndex(req.Index, td)
		if nerr != nil {
			return nil, nerr
		}
		for _, ix := range td.Indexes {
			if strings.EqualFold(ix.Name, spec.Name) {
				return nil, fmt.Errorf("%w: 索引 %q 已存在", ErrInvalidDDL, ix.Name)
			}
		}
		// 唯一索引前先查重复值：让用户拿到样本，而不是等引擎报一句无从下手的错。
		if spec.Unique {
			n, samples, perr := e.probeDuplicates(ctx, table, spec.Columns)
			if perr == nil && n > 0 {
				return nil, fmt.Errorf("%w：%d 组重复，例如 %s",
					ErrDuplicateRows, n, strings.Join(samples, "、"))
			}
		}
		plan.Statements = []PlanStatement{{
			SQL:        e.dialect.CreateIndexSQL(table, spec.Name, spec.Columns, spec.Unique),
			ExpectRows: -1,
		}}
		plan.Rollback = []PlanStatement{{
			SQL:        e.dialect.DropIndexSQL(table, spec.Name),
			ExpectRows: -1,
		}}
		// 大表上建索引会锁写，引擎各有各的在线做法。只给建议不自动生成：
		// PostgreSQL 的 CONCURRENTLY 不能在事务里跑，而执行门是单事务的。
		if advice := e.dialect.IndexBuildAdvice(table, spec.Columns, spec.Unique); advice != "" {
			plan.Notes = append(plan.Notes, advice)
		}
		plan.AffectedRows = 0

	case DDLDropIndex:
		if !e.conn.Capabilities().Indexes {
			return nil, fmt.Errorf("%w: 该引擎不支持索引管理", ErrInvalidDDL)
		}
		name, nerr := normalizeColumnName(req.Index.Name)
		if nerr != nil {
			return nil, fmt.Errorf("%w: 索引名 %q 不合法", ErrInvalidDDL, req.Index.Name)
		}
		var target *model.Index
		for i := range td.Indexes {
			if strings.EqualFold(td.Indexes[i].Name, name) {
				target = &td.Indexes[i]
			}
		}
		if target == nil {
			return nil, fmt.Errorf("%w: 索引 %q 不存在", ErrInvalidDDL, name)
		}
		if target.Primary {
			return nil, fmt.Errorf("%w: 主键索引不能删除", ErrInvalidDDL)
		}
		plan.Statements = []PlanStatement{{
			SQL:        e.dialect.DropIndexSQL(table, target.Name),
			ExpectRows: -1,
		}}
		// 回滚把索引按原定义建回来。删索引不动数据，所以不标不可逆。
		plan.Rollback = []PlanStatement{{
			SQL:        e.dialect.CreateIndexSQL(table, target.Name, target.Columns, target.Unique),
			ExpectRows: -1,
		}}
		plan.AffectedRows = 0

	case DDLAddForeignKey:
		if !e.conn.Capabilities().AddForeignKey {
			return nil, fmt.Errorf("%w: 该引擎不支持用 ALTER TABLE 追加外键（SQLite 只能重建整表）", ErrInvalidDDL)
		}
		fk, ferr := e.normalizeForeignKey(ctx, req, td)
		if ferr != nil {
			return nil, ferr
		}
		// 既有数据不满足约束时在这里就停手：让引擎去扫全表只会得到一句
		// 不带样本的报错，大表上还可能要持锁很久。
		pre, perr := e.PrecheckForeignKey(ctx, FKPrecheckRequest{
			Child:      req.Table,
			Parent:     parentRef(req.Table, fk.RefTable),
			Columns:    fk.Columns,
			RefColumns: fk.RefColumns,
		})
		if perr != nil {
			return nil, perr
		}
		if pre.Blocked {
			detail := pre.Reason
			if len(pre.Samples) > 0 {
				detail += "，例如 " + strings.Join(pre.Samples, "、")
			}
			return nil, fmt.Errorf("%w：%s", ErrForeignKeyBlocked, detail)
		}
		plan.Statements = []PlanStatement{{
			SQL: e.dialect.AddForeignKeySQL(table, fk.Name, fk.Columns,
				e.dialect.QuoteQualified(schemaOf(req.Table, e.cfgSchema()), fk.RefTable),
				fk.RefColumns, fk.OnDelete, fk.OnUpdate),
			ExpectRows: -1,
		}}
		// 回滚是把约束摘掉：不动数据，所以不标不可逆。
		plan.Rollback = []PlanStatement{{
			SQL:        e.dialect.DropForeignKeySQL(table, fk.Name),
			ExpectRows: -1,
		}}
		if td.RowEstimate > 0 {
			plan.AffectedRows = td.RowEstimate // 建约束会校验既有行
		} else {
			plan.AffectedRows = -1
		}

	case DDLDropTable:
		// 删除前先查引用：视图与外键都会在 DROP 之后才报错，而没有回滚。
		if deps, derr := e.conn.Meta().Dependents(ctx, req.Table); derr == nil && len(deps) > 0 {
			return nil, fmt.Errorf("%w：%s", ErrDependencyBlocked, describeDependents(deps))
		}
		plan.Statements = []PlanStatement{{
			SQL:        "DROP TABLE " + table,
			ExpectRows: -1,
		}}
		// 回滚只是补偿：结构能建回来，里面的数据回不来。
		plan.Rollback = []PlanStatement{{
			SQL:        e.recreateTableSQL(table, td),
			ExpectRows: -1,
		}}
		plan.Irreversible = true
		plan.Notes = append(plan.Notes,
			"删表不可逆：回滚语句只会把空表建回来，数据不会回来。需要保留数据请先导出。")
		plan.AffectedRows = -1

	default:
		return nil, fmt.Errorf("%w: 不支持的动作 %q", ErrInvalidDDL, req.Action)
	}

	// 预执行检查放在最后：此时语句已定，检查失败就直接作废这张预案。
	if err := e.checkBlockers(ctx, plan); err != nil {
		return nil, err
	}
	// 引擎的 DDL 在隐式提交时不参与事务，回滚语句只是补偿。
	e.finishDDLPlan(plan)
	return plan, nil
}

// fkActions 是外键动作的白名单。动作会被拼进 SQL，只能从这里取。
var fkActions = map[string]bool{
	"":          true,
	"CASCADE":   true,
	"RESTRICT":  true,
	"NO ACTION": true,
	"SET NULL":  true,
}

// parentRef 把一个表名补成父表引用，沿用子表的模式。
func parentRef(child model.ObjectRef, name string) model.ObjectRef {
	return model.ObjectRef{Kind: model.KindTable, Name: name, Schema: child.Schema}
}

// normalizeForeignKey 校验并规范化加外键请求。
func (e *Executor) normalizeForeignKey(ctx context.Context, req DDLRequest, child *model.TableDetail) (ForeignKeySpec, error) {
	fk, err := validateFKShape(req.Table, child, req.ForeignKey)
	if err != nil {
		return fk, err
	}

	// 父表必须存在，且这几列必须在里面。
	parent, err := e.conn.Meta().DescribeTable(ctx, parentRef(req.Table, fk.RefTable))
	if err != nil {
		return fk, err
	}
	refs := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		name, err := normalizeColumnName(c)
		if err != nil {
			return fk, err
		}
		col, ok := findColumn(parent, name)
		if !ok {
			return fk, fmt.Errorf("%w: 父表 %s 没有列 %q", ErrInvalidDDL, fk.RefTable, name)
		}
		refs[i] = col.Name
	}
	fk.RefTable = parent.Ref.Name
	fk.RefColumns = refs
	return fk, nil
}

// validateFKShape 是不碰数据库的那一半校验：形状、名字、动作、可空性。
// 单独拆出来的理由很实际——SQLite 不支持加外键，如果校验裹在能力检查后面，
// 这些规则在没有真机的环境里就一条都测不到。
func validateFKShape(childRef model.ObjectRef, child *model.TableDetail, in ForeignKeySpec) (ForeignKeySpec, error) {
	out := ForeignKeySpec{
		Name: strings.TrimSpace(in.Name), RefTable: strings.TrimSpace(in.RefTable),
	}

	if out.RefTable == "" {
		return out, fmt.Errorf("%w: 必须指定父表", ErrInvalidDDL)
	}
	if len(in.Columns) == 0 {
		return out, fmt.Errorf("%w: 至少要指定一列", ErrInvalidDDL)
	}
	if len(in.Columns) != len(in.RefColumns) {
		return out, fmt.Errorf("%w: 子表列与父表列数量不一致（%d vs %d）",
			ErrInvalidDDL, len(in.Columns), len(in.RefColumns))
	}

	cols := make([]string, len(in.Columns))
	for i, c := range in.Columns {
		name, err := normalizeColumnName(c)
		if err != nil {
			return out, err
		}
		col, ok := findColumn(child, name)
		if !ok {
			return out, fmt.Errorf("%w: 子表没有列 %q", ErrInvalidDDL, name)
		}
		cols[i] = col.Name
	}
	refCols := make([]string, len(in.RefColumns))
	for i, c := range in.RefColumns {
		name, err := normalizeColumnName(c)
		if err != nil {
			return out, err
		}
		refCols[i] = name
	}
	out.Columns, out.RefColumns = cols, refCols

	onDelete, err := normalizeFKAction("ON DELETE", in.OnDelete)
	if err != nil {
		return out, err
	}
	onUpdate, err := normalizeFKAction("ON UPDATE", in.OnUpdate)
	if err != nil {
		return out, err
	}
	out.OnDelete, out.OnUpdate = onDelete, onUpdate

	// SET NULL 要求引用列可空：MySQL 会直接拒绝，PG 会建成功但删除父行时才报错，
	// 后者更难查，所以统一在这里挡住。
	if onDelete == "SET NULL" || onUpdate == "SET NULL" {
		for _, name := range cols {
			if col, ok := findColumn(child, name); ok && !col.Nullable {
				return out, fmt.Errorf("%w: 列 %q 是 NOT NULL，不能用 SET NULL", ErrInvalidDDL, col.Name)
			}
		}
	}

	if out.Name == "" {
		out.Name = defaultFKName(childRef.Name, cols)
	}
	if _, err := normalizeColumnName(out.Name); err != nil {
		return out, fmt.Errorf("%w: 外键名 %q 不合法", ErrInvalidDDL, in.Name)
	}
	if len(out.Name) > 64 {
		return out, fmt.Errorf("%w: 外键名过长（上限 64 字符）", ErrInvalidDDL)
	}
	for _, existing := range child.ForeignKeys {
		if strings.EqualFold(existing.Name, out.Name) {
			return out, fmt.Errorf("%w: 外键 %q 已存在", ErrInvalidDDL, existing.Name)
		}
	}
	return out, nil
}

// normalizeFKAction 只放行白名单里的动作。
func normalizeFKAction(what, v string) (string, error) {
	act := strings.ToUpper(strings.Join(strings.Fields(strings.TrimSpace(v)), " "))
	if !fkActions[act] {
		return "", fmt.Errorf("%w: %s 只接受 CASCADE / RESTRICT / NO ACTION / SET NULL", ErrInvalidDDL, what)
	}
	return act, nil
}

// defaultFKName 在没有给名字时生成一个确定的名字。
// 确定性很重要：回滚要靠这个名字摘约束，随机名会让撤销无从下手。
func defaultFKName(table string, cols []string) string {
	name := "fk_" + table + "_" + strings.Join(cols, "_")
	if len(name) <= 64 {
		return name
	}
	// 太长就截断再补一个短哈希，避免不同外键撞名。
	sum := sha256.Sum256([]byte(name))
	return name[:55] + "_" + hex.EncodeToString(sum[:4])
}

// blockColumnDependents 拦住「删一列，但还有外键引用这一列」。
//
// 引擎会拒绝，但报错只说约束名不合法或外键冲突，不会告诉你上游是谁；
// 在这里挡下并列出引用方，用户才知道该先改哪张表。
func (e *Executor) blockColumnDependents(ctx context.Context, ref model.ObjectRef, column string) error {
	deps, err := e.conn.Meta().Dependents(ctx, ref)
	if err != nil {
		return nil // 查不到就不阻断：宁可让引擎报错，也不要因为元数据查询失败挡住正常操作
	}
	var hit []model.Dependent
	for _, d := range deps {
		if d.Kind != "foreign_key" {
			continue
		}
		for _, c := range d.Columns {
			if strings.EqualFold(c, column) {
				hit = append(hit, d)
				break
			}
		}
	}
	if len(hit) == 0 {
		return nil
	}
	return fmt.Errorf("%w：%s", ErrDependencyBlocked, describeDependents(hit))
}

// viewDependents 返回引用该表的视图名（只用于提示，不阻断）。
func (e *Executor) viewDependents(ctx context.Context, ref model.ObjectRef) []string {
	deps, err := e.conn.Meta().Dependents(ctx, ref)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range deps {
		if d.Kind == "view" {
			out = append(out, d.Name)
		}
	}
	return out
}

// describeDependents 把依赖列表写成一句能照着做的话。
// 近似结果必须显式标注，否则用户会以为那是精确结论。
func describeDependents(deps []model.Dependent) string {
	parts := make([]string, 0, len(deps))
	approx := false
	for _, d := range deps {
		label := d.Name
		if d.Kind == "view" {
			label = "视图 " + d.Name
		} else if d.Kind == "foreign_key" {
			label = "外键 " + d.Name
		}
		if d.Detail != "" {
			label += "（" + d.Detail + "）"
		}
		if !d.Exact {
			approx = true
			label += "[按文本匹配得到，可能误报]"
		}
		parts = append(parts, label)
	}
	out := strings.Join(parts, "；")
	if approx {
		out += "。近似匹配可能包含无关对象，确认不需要它们之后再删"
	}
	return out
}

// recreateTableSQL 用元数据拼一条补偿用的建表语句。
//
// 它只保证「结构大致回来」：约束、注释、引擎特有项都可能丢失，
// 所以调用方必须同时标 Irreversible。不假装能还原。
func (e *Executor) recreateTableSQL(table string, td *model.TableDetail) string {
	cols := make([]string, 0, len(td.Columns))
	for _, c := range td.Columns {
		def := e.columnDefinitionFromColumn(c)
		cols = append(cols, def)
	}
	if pk := td.PrimaryKeyColumns(); len(pk) > 0 {
		cols = append(cols, "PRIMARY KEY ("+strings.Join(e.quoteAll(pk), ", ")+")")
	}
	return "CREATE TABLE " + table + " (" + strings.Join(cols, ", ") + ")"
}

// ErrLockWait 表示当前已经有会话在等锁，此时不该再执行拿锁的 DDL。
var ErrLockWait = errors.New("当前有会话在等锁，先处理锁等待再执行结构变更")

// longTransactionThreshold 是「多久算长事务」。60 秒：低于它的写事务在正常
// 业务里很常见，拿它去提示只会变成噪音。
const longTransactionThreshold = 60 * time.Second

// checkBlockers 做预执行检查：长事务只提示，锁等待才阻断。
//
// 这条不对称是刻意的：忙碌的库上永远有人开着事务，用长事务阻断会让工具
// 完全没法用；而「已经有人在等锁」说明再上一条拿锁的 DDL 只会让队列更长，
// 那种情况下不该假装一切正常。
func (e *Executor) checkBlockers(ctx context.Context, plan *ChangePlan) error {
	blockers, err := e.conn.Meta().Blockers(ctx, longTransactionThreshold)
	if err != nil {
		// 查不到就不检查：元数据查询失败不该变成「不让你改结构」。
		return nil
	}
	waits, notes := splitBlockers(blockers, 3)
	for _, n := range notes {
		plan.Notes = append(plan.Notes, "执行前检查："+n)
	}
	if len(waits) > 0 {
		return fmt.Errorf("%w：%s", ErrLockWait, strings.Join(waits, "；"))
	}
	return nil
}

// splitBlockers 把阻塞项拆成「阻断」与「提示」两堆。
//
// 单独提出来是为了让这条规则可以被单测直接验证——它是本模块里唯一一处
// 「同样是阻塞项，有的挡有的不挡」的地方，写错了要么把工具锁死，要么形同虚设。
func splitBlockers(blockers []model.Blocker, maxNotes int) (waits, notes []string) {
	for _, b := range blockers {
		if b.Kind == "lock_wait" {
			waits = append(waits, b.Detail)
			continue
		}
		if len(notes) >= maxNotes {
			continue
		}
		notes = append(notes, b.Detail)
	}
	if len(blockers)-len(waits) > maxNotes {
		notes = append(notes, fmt.Sprintf("另有 %d 个长事务未逐条列出", len(blockers)-len(waits)-maxNotes))
	}
	return waits, notes
}

// finishDDLPlan 补上风险、摩擦与回滚语义。// 结构变更与数据变更共用同一套分级：不因为「是 DDL」就给个特例等级。
func (e *Executor) finishDDLPlan(plan *ChangePlan) {
	plan.RollsBackAsCompensation = !e.conn.Capabilities().DDLTransactional

	var highest policy.Level
	for _, st := range plan.Statements {
		if g := policy.GradeStatement(e.dialect, st.SQL, nil); g.Level > highest {
			highest = g.Level
		}
	}
	plan.Risk = highest
	plan.Friction = policy.FrictionFor(e.env, highest, policy.Modifiers{
		ChangeSetPath:      true,
		StatementCount:     len(plan.Statements),
		RowsAffected:       plan.AffectedRows,
		ReadOnlyConnection: !e.mode.AllowsWrite(),
	})
}

// buildCreateTablePlan 生成建表预案。
//
// v1 要求必须给主键：没有主键的新表在本工具里既不能浏览（键集分页）也不能改，
// 建出来只会让人困惑。这条约束写在预案校验里，而不是事后提示。
func (e *Executor) buildCreateTablePlan(ctx context.Context, req DDLRequest) (*ChangePlan, error) {
	name := strings.TrimSpace(req.Table.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: 表名不能为空", ErrInvalidDDL)
	}
	if len(req.Columns) == 0 {
		return nil, fmt.Errorf("%w: 至少要有一列", ErrInvalidDDL)
	}
	if len(req.PrimaryKey) == 0 {
		return nil, fmt.Errorf("%w: 必须指定主键（无主键的新表在本工具里不可浏览、不可编辑）", ErrInvalidDDL)
	}

	specs := make([]ColumnSpec, 0, len(req.Columns))
	byName := make(map[string]int, len(req.Columns))
	for _, c := range req.Columns {
		spec, err := normalizeColumn(c)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(spec.Name)
		if _, dup := byName[key]; dup {
			return nil, fmt.Errorf("%w: 列 %q 重复", ErrInvalidDDL, spec.Name)
		}
		byName[key] = len(specs)
		specs = append(specs, spec)
	}

	// 主键列必须真实存在；同时主键列一律 NOT NULL，让引擎与工具对「能不能为空」有同一个答案。
	pkNames := make([]string, 0, len(req.PrimaryKey))
	pkSeen := make(map[string]bool, len(req.PrimaryKey))
	for _, pk := range req.PrimaryKey {
		canonical, err := normalizeColumnName(pk)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(canonical)
		idx, ok := byName[key]
		if !ok {
			return nil, fmt.Errorf("%w: 主键列 %q 不在列清单里", ErrInvalidDDL, canonical)
		}
		if pkSeen[key] {
			return nil, fmt.Errorf("%w: 主键列 %q 重复", ErrInvalidDDL, canonical)
		}
		pkSeen[key] = true
		specs[idx].Nullable = false
		pkNames = append(pkNames, specs[idx].Name)
	}

	exists, err := e.tableExists(ctx, req.Table)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("%w: 表 %q 已存在", ErrInvalidDDL, name)
	}

	table := e.dialect.QuoteQualified(schemaOf(req.Table, e.cfgSchema()), name)
	defs := make([]string, 0, len(specs)+1)
	for _, s := range specs {
		defs = append(defs, e.columnDefinition(s))
	}
	pkCols := make([]string, 0, len(pkNames))
	for _, c := range pkNames {
		pkCols = append(pkCols, e.dialect.QuoteIdent(c))
	}
	defs = append(defs, "PRIMARY KEY ("+strings.Join(pkCols, ", ")+")")

	plan := &ChangePlan{
		ID:        "ddl-" + newID(),
		Table:     model.ObjectRef{Kind: model.KindTable, Name: name, Schema: req.Table.Schema, Catalog: req.Table.Catalog},
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(planTTL),
		DDL:       true,
		// 建表没有旧定义可比，改为要求「执行时表仍然不存在」。
		ExpectTableAbsent: true,
		Statements: []PlanStatement{{
			SQL:        "CREATE TABLE " + table + " (" + strings.Join(defs, ", ") + ")",
			ExpectRows: -1,
		}},
		Rollback: []PlanStatement{{
			SQL:        "DROP TABLE " + table,
			ExpectRows: -1,
		}},
		AffectedRows: 0,
	}
	e.finishDDLPlan(plan)
	return plan, nil
}

// buildRawDDLPlan 把手写 DDL 纳入同一套 Plan/Apply。
//
// 三条刻意的限制：
//   - 一次只允许一条语句。多条没法保证「要么都成、要么都不成」——MySQL 的
//     DDL 隐式提交，第二条失败时第一条已经生效，而回滚语句是无处生成的。
//   - 只接受 DDL。DML 有自己的变更集路径（带乐观并发与逐行回滚），
//     混进来会绕开那些保护。
//   - **不做语义预检**。收窄、孤儿、依赖这些检查都要理解语句含义，而手写
//     DDL 我们不解析。预案里必须写明这一点，并且标不可逆——不假装能回来。
func (e *Executor) buildRawDDLPlan(ctx context.Context, req DDLRequest) (*ChangePlan, error) {
	raw := strings.TrimSpace(req.RawSQL)
	if raw == "" {
		return nil, fmt.Errorf("%w: 手写 DDL 不能为空", ErrInvalidDDL)
	}

	stmts, err := e.dialect.Split(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: 语句切分失败: %v", ErrInvalidDDL, err)
	}
	if len(stmts) != 1 {
		return nil, fmt.Errorf("%w: 一次只能提交一条 DDL（当前 %d 条）；"+
			"多条无法保证原子性，请拆开逐条执行", ErrInvalidDDL, len(stmts))
	}
	text := strings.TrimSpace(stmts[0].Text)

	class, err := e.dialect.Classify(text)
	if err != nil {
		return nil, fmt.Errorf("%w: 无法识别语句类型: %v", ErrInvalidDDL, err)
	}
	if class != dialect.ClassDDL {
		return nil, fmt.Errorf("%w: 只接受结构变更语句（DDL），当前是 %s；"+
			"改数据请用变更集，它带乐观并发与逐行回滚", ErrInvalidDDL, class)
	}

	plan := &ChangePlan{
		ID:           "ddl-raw-" + newID(),
		Table:        req.Table,
		CreatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(planTTL),
		DDL:          true,
		Irreversible: true,
		Statements:   []PlanStatement{{SQL: text, ExpectRows: -1}},
		AffectedRows: -1,
	}
	plan.Notes = append(plan.Notes,
		"手写 DDL 未做语义预检：收窄、孤儿数据、依赖关系都没有检查，执行前请自行确认。",
		"手写 DDL 不生成回滚语句——工具不知道这条语句的反向操作是什么。",
	)

	// 竞态保护只在「指明了表且表存在」时给：拿不到基准定义就别假装有。
	if strings.TrimSpace(req.Table.Name) != "" {
		if cur, derr := e.conn.Meta().DDL(ctx, req.Table); derr == nil {
			plan.ExpectDDLHash = ddlHash(cur)
		}
	}

	if err := e.checkBlockers(ctx, plan); err != nil {
		return nil, err
	}
	e.finishDDLPlan(plan)
	return plan, nil
}

// normalizeViewQuery 校验视图主体。
//
// 视图定义没法参数化，只能拼；所以按「是不是一条查询」卡：
//   - 必须以 SELECT 或 WITH 开头（`CREATE VIEW v AS DELETE …` 这种在引擎侧报错，
//     错误信息指不到我们的输入上）；
//   - 不能有分号：分号会被切分器切成两条语句，第二条的执行结果没人看；
//   - 不能有控制字符（换行与制表除外），长度上限 4000。
//
// 不做的是语法分析：视图能不能建，最终由引擎判定。
func normalizeViewQuery(q string) (string, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", fmt.Errorf("%w: 视图定义不能为空", ErrInvalidDDL)
	}
	if len(q) > 4000 {
		return "", fmt.Errorf("%w: 视图定义过长（>4000 字符）", ErrInvalidDDL)
	}
	if strings.Contains(q, ";") {
		return "", fmt.Errorf("%w: 视图定义里不能有分号", ErrInvalidDDL)
	}
	for _, r := range q {
		if r < 0x20 && r != 0x0a && r != 0x09 && r != 0x0d {
			return "", fmt.Errorf("%w: 视图定义里有控制字符", ErrInvalidDDL)
		}
	}
	head := strings.ToLower(q)
	if !strings.HasPrefix(head, "select") && !strings.HasPrefix(head, "with") {
		return "", fmt.Errorf("%w: 视图定义必须以 SELECT 或 WITH 开头", ErrInvalidDDL)
	}
	return q, nil
}

// buildCreateViewPlan 生成建视图预案。
//
// 同名视图存在时直接拒绝而不是加 OR REPLACE：替换掉一个已有视图，等于悄悄改掉
// 所有依赖它的查询看到的列，这该由人来决定。
func (e *Executor) buildCreateViewPlan(ctx context.Context, req DDLRequest) (*ChangePlan, error) {
	if !e.conn.Capabilities().Views {
		return nil, fmt.Errorf("%w: 该引擎不支持视图", ErrInvalidDDL)
	}
	name := strings.TrimSpace(req.View.Name)
	if !constraintNamePattern.MatchString(name) {
		return nil, fmt.Errorf("%w: 视图名 %q 只能用字母、数字、下划线，且以字母或下划线开头",
			ErrInvalidDDL, req.View.Name)
	}
	query, qerr := normalizeViewQuery(req.View.Query)
	if qerr != nil {
		return nil, qerr
	}
	if objs, oerr := e.conn.Meta().Objects(ctx, req.Table.Catalog, req.Table.Schema, model.KindView); oerr == nil {
		for _, o := range objs {
			if strings.EqualFold(o.Name, name) {
				return nil, fmt.Errorf("%w: 视图 %q 已存在（要改它请先删，或用「手写 DDL」）", ErrInvalidDDL, name)
			}
		}
	}
	quoted := e.dialect.QuoteQualified(schemaOf(req.Table, e.cfgSchema()), name)
	plan := &ChangePlan{
		ID: "ddl-" + newID(),
		Table: model.ObjectRef{
			Kind: model.KindView, Name: name, Schema: req.Table.Schema, Catalog: req.Table.Catalog,
		},
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(planTTL),
		Statements: []PlanStatement{{
			SQL: e.dialect.CreateViewSQL(quoted, query), ExpectRows: -1,
		}},
		Rollback: []PlanStatement{{
			SQL: e.dialect.DropViewSQL(quoted), ExpectRows: -1,
		}},
		AffectedRows: 0,
	}
	plan.Notes = append(plan.Notes, "视图只存查询定义，不复制数据；删除视图不影响底层表")
	e.finishDDLPlan(plan)
	return plan, nil
}
