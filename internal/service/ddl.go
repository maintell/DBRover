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
	DDLAddColumn   DDLAction = "add_column"
	DDLDropColumn  DDLAction = "drop_column"
	DDLCreateTable DDLAction = "create_table"
)

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
	if c.Default != nil {
		if d := strings.TrimSpace(*c.Default); defaultPattern.MatchString(d) {
			parts = append(parts, "DEFAULT", d)
		}
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

	// 删列只需要列名，类型与默认值不参与语句，因此不必校验。
	var spec ColumnSpec
	if req.Action == DDLDropColumn {
		name, nerr := normalizeColumnName(req.Column.Name)
		if nerr != nil {
			return nil, nerr
		}
		spec = ColumnSpec{Name: name}
	} else {
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

	default:
		return nil, fmt.Errorf("%w: 不支持的动作 %q", ErrInvalidDDL, req.Action)
	}

	// 引擎的 DDL 在隐式提交时不参与事务，回滚语句只是补偿。
	e.finishDDLPlan(plan)
	return plan, nil
}

// finishDDLPlan 补上风险、摩擦与回滚语义。
// 结构变更与数据变更共用同一套分级：不因为「是 DDL」就给个特例等级。
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
