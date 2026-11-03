package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
)

// ErrSchemaChanged 表示表定义在预案生成后发生了变化。
// 结构和数据不一样：拿旧结构去改已被别人改过的表，生成的语句可能直接不合法。
var ErrSchemaChanged = errors.New("表定义已变化，请重新生成结构变更预案")

// ErrInvalidDDL 表示结构变更参数不合法（列名 / 类型 / 默认值）。
var ErrInvalidDDL = errors.New("结构变更参数不合法")

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
)

// ErrForeignKeyBlocked 表示既有数据不满足要加的外键（有孤儿行，或父表列不唯一）。
var ErrForeignKeyBlocked = errors.New("既有数据不满足该外键，加约束会失败")

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
}

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

// countNonNull 数出某列的非空行数，用于告知「删这一列会丢多少数据」。
// 查不出来就返回 -1（未知），绝不返回 0 假装「没有数据」。
func (e *Executor) countNonNull(ctx context.Context, table, column string) int64 {
	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return -1
	}
	defer func() { _ = sess.Close() }()

	q := "SELECT COUNT(*) FROM " + table + " WHERE " + e.dialect.QuoteIdent(column) + " IS NOT NULL"
	cur, err := sess.Query(ctx, q, nil, driver.QueryOptions{})
	if err != nil {
		return -1
	}
	defer func() { _ = cur.Close() }()

	rows, _, err := cur.Next(ctx)
	if err != nil || len(rows) != 1 || len(rows[0]) == 0 {
		return -1
	}
	switch v := rows[0][0].(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case int:
		return int64(v)
	case []byte:
		if n, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return n
		}
	}
	return -1
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

	default:
		return nil, fmt.Errorf("%w: 不支持的动作 %q", ErrInvalidDDL, req.Action)
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
