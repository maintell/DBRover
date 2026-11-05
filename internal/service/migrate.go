package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/maintell/DBRover/internal/driver/dialect"
	"github.com/maintell/DBRover/internal/model"
)

// MigrateStatus 是一列的迁移判定（docs/08 §4.3 的规则落地）。
type MigrateStatus string

const (
	// MigrateOK：直接可迁，无需人看。
	MigrateOK MigrateStatus = "ok"
	// MigrateReview：能迁但有语义漂移（精度、时区、类型含义），必须人确认。
	MigrateReview MigrateStatus = "review"
	// MigrateBlocked：迁过去会丢数据或根本表达不了，不允许自动放行。
	MigrateBlocked MigrateStatus = "blocked"
)

// ColumnVerdict 是一列的迁移结论。
type ColumnVerdict struct {
	Column string        `json:"column"`
	Source string        `json:"source_type"`
	Target string        `json:"target_hint,omitempty"`
	Status MigrateStatus `json:"status"`
	Reason string        `json:"reason,omitempty"`
	// SourceMaxLen / TargetMaxLen 是声明长度（0 表示没有声明，无从比较）。
	SourceMaxLen int `json:"source_max_len,omitempty"`
	TargetMaxLen int `json:"target_max_len,omitempty"`
	// ActualMax 是 plan -probe 读出来的实际最大字符数；没探测时为 nil。
	ActualMax *int64 `json:"actual_max_len,omitempty"`
}

// MigratePlan 是一张表的预检结果。
//
// 只回答「能不能迁、哪里要人看」，不搬数据、不生成 DDL——那是后续步骤。
type MigratePlan struct {
	Columns []ColumnVerdict `json:"columns"`
	Blocked int             `json:"blocked"`
	Review  int             `json:"review"`
}

// Safe 表示没有任何需要人判断或阻断的列。
func (p *MigratePlan) Safe() bool { return p.Blocked == 0 && p.Review == 0 }

// PlanMigration 逐列给出迁移结论（docs/08 §4.3）。
//
// dst 为 nil 表示目标表还不存在（要新建）：此时只做源侧的语义提示，
// 不与已有类型做收窄比较——没有目标结构可比。
func PlanMigration(src, dst *model.TableDetail, srcEngine, dstEngine model.Engine) *MigratePlan {
	plan := &MigratePlan{}
	dstByName := map[string]model.Column{}
	if dst != nil {
		for _, c := range dst.Columns {
			dstByName[strings.ToLower(c.Name)] = c
		}
	}
	for _, sc := range src.Columns {
		var dc *model.Column
		if d, ok := dstByName[strings.ToLower(sc.Name)]; ok {
			dc = &d
		}
		v := verdictFor(sc, dc, srcEngine, dstEngine)
		switch v.Status {
		case MigrateBlocked:
			plan.Blocked++
		case MigrateReview:
			plan.Review++
		}
		plan.Columns = append(plan.Columns, v)
	}
	// 目标多出来的列也值得提一句：迁移不会删它们，但源里没有数据。
	for _, dc := range dstByName {
		found := false
		for _, sc := range src.Columns {
			if strings.EqualFold(sc.Name, dc.Name) {
				found = true
				break
			}
		}
		if !found {
			plan.Columns = append(plan.Columns, ColumnVerdict{
				Column: dc.Name, Target: dc.Type.Raw, Status: MigrateReview,
				Reason: "目标表有这一列、源表没有：迁移不会写它，请确认默认值是否合适",
			})
			plan.Review++
		}
	}
	return plan
}

func verdictFor(sc model.Column, dc *model.Column, srcEngine, dstEngine model.Engine) ColumnVerdict {
	// 生成列必须最先拦：元数据里**没有**派生表达式（model.Column 只有 Default），
	// 按普通列迁过去会得到一个空列——源里算好的值变成 NULL，而摘要校验发现不了
	// （两端用同一份列清单），属于「看起来迁完了、数据不一样」。
	// 唯一可接受的做法是让人给出目标类型，把派生值固化成普通列。
	if sc.IsGenerated {
		return ColumnVerdict{
			Column: sc.Name,
			Source: sc.Type.Raw,
			Status: MigrateBlocked,
			Reason: "生成列：派生表达式不在元数据里，直接迁会建成空列。若接受「把计算结果固化成普通列」，" +
				"给出目标类型即可（决定后值会被复制过去）",
		}
	}
	v := ColumnVerdict{Column: sc.Name, Source: sc.Type.Raw, SourceMaxLen: sc.Type.MaxLen}
	if dc != nil {
		v.Target = dc.Type.Raw
		v.TargetMaxLen = dc.Type.MaxLen
	}
	raw := strings.ToLower(strings.TrimSpace(sc.Type.Raw))

	// 1) 收窄一律阻断：这是唯一能确定「会丢数据」的一类。
	if dc != nil {
		if why := narrowing(raw, sc, *dc); why != "" {
			v.Status, v.Reason = MigrateBlocked, why
			return v
		}
	}

	// 2) 同引擎：源类型在目标端本来就合法，直接放行。
	//
	// dc == nil 表示目标表还不存在（要新建）——同引擎时源类型拿来建表是对的，
	// 不该因为「没有目标结构可比」就要求人逐列决定。
	if srcEngine == dstEngine && (dc == nil || strings.EqualFold(strings.TrimSpace(sc.Type.Raw), strings.TrimSpace(dc.Type.Raw))) {
		v.Status = MigrateOK
		if v.Target == "" {
			v.Target = strings.TrimSpace(sc.Type.Raw)
		}
		return v
	}

	// 3) 跨引擎的语义规则（节选自 docs/08 §4.3 的表）。
	switch srcEngine {
	case model.EngineMySQL:
		switch {
		case strings.HasPrefix(raw, "tinyint(1)"):
			v.Status = MigrateReview
			v.Target = firstNonEmpty(v.Target, "boolean")
			v.Reason = "TINYINT(1) 可能被当作数值而不是布尔；映射成 boolean 前请确认实际用法"
			return v
		case strings.HasPrefix(raw, "bigint") && strings.Contains(raw, "unsigned"):
			v.Status = MigrateReview
			v.Target = firstNonEmpty(v.Target, "numeric(20)")
			v.Reason = "目标库没有无符号整数；映射成 bigint 会溢出，需用 numeric(20)"
			return v
		case strings.HasPrefix(raw, "datetime"):
			// 无时区 → 无时区：保真。要提醒的是别映射成 timestamptz（会引入偏移）。
			v.Status = MigrateOK
			v.Target = firstNonEmpty(v.Target, "timestamp")
			return v
		case strings.HasPrefix(raw, "timestamp"):
			v.Status = MigrateReview
			v.Reason = "MySQL TIMESTAMP 有 2038 上限；目标端若用更宽类型，注意历史数据含义"
			return v
		case strings.HasPrefix(raw, "enum") || strings.HasPrefix(raw, "set"):
			v.Status = MigrateReview
			v.Target = firstNonEmpty(v.Target, "text + CHECK")
			v.Reason = "ENUM/SET 在目标库没有直接对应；建议 text + CHECK，原生 enum 增减值需 ALTER TYPE"
			return v
		}
	case model.EnginePostgres:
		switch {
		case strings.HasSuffix(raw, "[]"):
			v.Status = MigrateBlocked
			v.Reason = "目标库没有数组类型；需拆子表或改 JSON，这是结构决策，不能自动映射"
			return v
		case raw == "jsonb" && dstEngine == model.EngineMySQL:
			v.Status = MigrateReview
			v.Target = firstNonEmpty(v.Target, "json")
			v.Reason = "MySQL 的 JSON 保真存值，但查询能力与 jsonb 不同（无 GIN 索引）"
			return v
		case raw == "uuid" && dstEngine == model.EngineMySQL:
			v.Status = MigrateReview
			v.Target = firstNonEmpty(v.Target, "char(36)")
			v.Reason = "MySQL 没有 uuid 类型；char(36) 索引体积更大，binary(16) 需要转换逻辑"
			return v
		}
	case model.EngineOracle:
		switch {
		case raw == "number":
			v.Status = MigrateReview
			v.Reason = "Oracle NUMBER 不带精度时是浮点语义；映射前请确认实际取值范围"
			return v
		case raw == "date":
			v.Status = MigrateReview
			v.Target = firstNonEmpty(v.Target, "timestamp")
			v.Reason = "Oracle 的 DATE 含时分秒；映射成 date 会丢时间"
			return v
		case strings.HasPrefix(raw, "varchar2"), strings.HasPrefix(raw, "nvarchar2"):
			v.Status = MigrateReview
			v.Reason = "Oracle 的长度是字节还是字符取决于语义（CHAR/VARCHAR2），迁移前要核对"
			return v
		}
	case model.EngineSQLite:
		v.Status = MigrateReview
		v.Reason = "SQLite 类型弱（亲和性）；只看声明类型不够，需抽样看实际值"
		return v
	}

	// 4) 没命中任何规则：不假装安全。
	v.Status = MigrateReview
	v.Reason = "没有已知映射规则；请人工确认目标类型"
	return v
}

// narrowing 只在「同类且目标更窄」时给结论，其余返回空串。
func narrowing(raw string, sc model.Column, dc model.Column) string {
	if sc.Type.Kind != dc.Type.Kind {
		return ""
	}
	if sc.Type.MaxLen > 0 && dc.Type.MaxLen > 0 && sc.Type.MaxLen > dc.Type.MaxLen {
		return fmt.Sprintf("源长度 %d 超过目标上限 %d，会截断", sc.Type.MaxLen, dc.Type.MaxLen)
	}
	if sc.Type.Precision > 0 && dc.Type.Precision > 0 {
		if sc.Type.Precision > dc.Type.Precision {
			return fmt.Sprintf("源精度 %d 超过目标精度 %d，会丢有效数字", sc.Type.Precision, dc.Type.Precision)
		}
		if sc.Type.Scale > dc.Type.Scale {
			return fmt.Sprintf("源小数位 %d 超过目标 %d，会丢精度", sc.Type.Scale, dc.Type.Scale)
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ErrMigrateNeedsDecision 表示还有列没拿到人工决定的目标类型。
var ErrMigrateNeedsDecision = errors.New("迁移：仍有列需要人工决定目标类型")

// BuildMigrationDDL 把预检结论渲染成目标端的 CREATE TABLE 语句。
//
// 规则：**review 与 blocked 的列必须在 decisions 里给出目标类型**，否则直接报错
// 并点名——「默认放行」正是迁移里最危险的失败方式。ok 的列用预检给出的类型。
//
// 只生成单表结构：索引、外键、默认值都不在这里（它们要按目标端能力逐个决定）。
func BuildMigrationDDL(dstEngine model.Engine, table string, src *model.TableDetail, plan *MigratePlan, decisions map[string]string) (string, error) {
	if src == nil || plan == nil {
		return "", errors.New("迁移：缺少源结构或预检结论")
	}
	byName := map[string]ColumnVerdict{}
	for _, v := range plan.Columns {
		byName[strings.ToLower(v.Column)] = v
	}

	d := dialect.For(dstEngine)
	var cols []string
	var missing []string
	for _, sc := range src.Columns {
		v, okVerdict := byName[strings.ToLower(sc.Name)]
		if !okVerdict {
			missing = append(missing, sc.Name+"（预检里没有结论）")
			continue
		}
		tedType := strings.TrimSpace(decisions[sc.Name])
		if tedType == "" {
			if v.Status != MigrateOK {
				missing = append(missing, fmt.Sprintf("%s（%s）", sc.Name, v.Status))
				continue
			}
			tedType = strings.TrimSpace(v.Target)
			if tedType == "" {
				tedType = strings.TrimSpace(sc.Type.Raw)
			}
		}
		if !typePattern.MatchString(tedType) {
			return "", fmt.Errorf("迁移：列 %s 的目标类型 %q 不合法（只允许类型名与长度/精度）", sc.Name, tedType)
		}
		spec := d.QuoteIdent(sc.Name) + " " + tedType
		if !sc.Nullable {
			spec += " NOT NULL"
		}
		// 默认值只搬字面量：表达式（nextval / now / 各种函数）跨引擎不等价，
		// 搬错会悄悄改变写入数据的含义（ADR-0012 §2）。
		if sc.Default != nil && defaultPattern.MatchString(strings.TrimSpace(*sc.Default)) {
			spec += " DEFAULT " + strings.TrimSpace(*sc.Default)
		}
		cols = append(cols, spec)
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("%w：%s", ErrMigrateNeedsDecision, strings.Join(missing, "、"))
	}
	if len(cols) == 0 {
		return "", errors.New("迁移：源表没有列")
	}
	// 主键必须一起建：没有主键的目标表既不能做键集续跑，也没法按行定位。
	// 索引不能塞进这条语句（DDLRaw 只接受单条），由 BuildMigrationIndexDDL 单独给。
	if pk := src.PrimaryKeyColumns(); len(pk) > 0 {
		quoted := make([]string, len(pk))
		for i, c := range pk {
			quoted[i] = d.QuoteIdent(c)
		}
		cols = append(cols, "PRIMARY KEY ("+strings.Join(quoted, ", ")+")")
	}
	// 唯一约束写成**匿名** UNIQUE：PG 的约束会隐式建同名索引，沿用源约束名
	// 可能在目标 schema 里撞名（ADR-0012 §1）。
	for _, ix := range src.Indexes {
		if ix.Primary || !ix.Constraint || !ix.Unique || len(ix.Columns) == 0 {
			continue
		}
		quoted := make([]string, len(ix.Columns))
		for i, c := range ix.Columns {
			quoted[i] = d.QuoteIdent(c)
		}
		cols = append(cols, "UNIQUE ("+strings.Join(quoted, ", ")+")")
	}
	return "CREATE TABLE " + d.QuoteIdent(table) + " (\n  " + strings.Join(cols, ",\n  ") + "\n)", nil
}

// BuildMigrationIndexDDL 生成目标端的建索引语句（不含主键与约束索引）。
//
// 单独一个函数是因为 DDLRaw 只接受**一条**语句：把 CREATE INDEX 拼在 CREATE TABLE
// 后面的文本里，执行门会以「必须恰好一条」拒绝。
func BuildMigrationIndexDDL(dstEngine model.Engine, table string, src *model.TableDetail) []string {
	if src == nil {
		return nil
	}
	d := dialect.For(dstEngine)
	quotedTable := d.QuoteIdent(table)
	var out []string
	for _, ix := range src.Indexes {
		if ix.Primary || ix.Constraint || len(ix.Columns) == 0 {
			continue
		}
		out = append(out, d.CreateIndexSQL(quotedTable, ix.Name, ix.Columns, ix.Unique)+";")
	}
	return out
}

// PlanMigrationBetween 读两端结构，给出迁移预检与目标端 DDL。
//
// dst 为 nil 表示目标表还不存在（要新建）：此时没有目标结构可比，收窄检查
// 会跳过——调用方要清楚这一点，别把「没检查」当成「检查通过」。
//
// 返回的 err 可能是 ErrMigrateNeedsDecision：这时 plan 仍然有效，调用方应当
// 展示逐列结论并要人给目标类型，而不是把它当成失败。
func PlanMigrationBetween(ctx context.Context, src, dst *Executor, table, targetTable string, decisions map[string]string) (*MigratePlan, string, error) {
	if src == nil {
		return nil, "", errors.New("迁移：缺少源执行器")
	}
	srcDetail, err := src.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: table})
	if err != nil {
		return nil, "", fmt.Errorf("读源表结构: %w", err)
	}
	name := targetTable
	if name == "" {
		name = table
	}
	var dstDetail *model.TableDetail
	var dstEngine model.Engine
	if dst != nil {
		dstEngine = dst.conn.Engine()
		if d, derr := dst.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: name}); derr == nil {
			dstDetail = d
		}
	} else {
		return nil, "", errors.New("迁移：缺少目标执行器")
	}

	plan := PlanMigration(srcDetail, dstDetail, src.conn.Engine(), dstEngine)
	ddlText, derr := BuildMigrationDDL(dstEngine, name, srcDetail, plan, decisions)
	return plan, ddlText, derr
}

// BuildMigrationNotes 列出「不会自动迁移、需要人工补」的东西。
//
// 这些缺口必须说出来：迁移完看起来一切正常，但唯一性、默认值、注释、外键
// 全都可能已经悄悄丢了（ADR-0012）。
func BuildMigrationNotes(src *model.TableDetail) []string {
	if src == nil {
		return nil
	}
	var notes []string
	for _, c := range src.Columns {
		if c.Default != nil && strings.TrimSpace(*c.Default) != "" &&
			!defaultPattern.MatchString(strings.TrimSpace(*c.Default)) {
			notes = append(notes, fmt.Sprintf("列 %s 的默认值未迁移（表达式不受支持）：%s",
				c.Name, strings.TrimSpace(*c.Default)))
		}
	}
	// 注释由 BuildMigrationCommentDDL 单独处理，这里不再重复提示。
	if n := len(src.ForeignKeys); n > 0 {
		notes = append(notes, fmt.Sprintf("外键（%d 个）不随建表创建：数据搬完后按目标库情况补建（见 migrate run 的输出）", n))
	}
	return notes
}

// BuildMigrationForeignKeyDDL 生成补建外键的语句（ADR-0013）。
//
// 三条规则：引擎不支持就不建（SQLite）；被引用表不在**目标库**就跳过并点名；
// 其余按源动作子句原样建。exists 由调用方提供，避免 service 直接连库判断。
//
// 为什么放在数据搬完之后：外键会挡住插入顺序。先建约束再搬数据，父子顺序稍错
// 就会被拒；搬完再补建，顺序问题自然消失。
func BuildMigrationForeignKeyDDL(dstEngine model.Engine, supportsFK bool, table string,
	src *model.TableDetail, exists func(name string) bool) (stmts []string, skipped []string) {
	if src == nil || len(src.ForeignKeys) == 0 {
		return nil, nil
	}
	d := dialect.For(dstEngine)
	quotedTable := d.QuoteIdent(table)
	for _, fk := range src.ForeignKeys {
		name := fk.Name
		if strings.TrimSpace(name) == "" {
			name = defaultFKName(table, fk.Columns)
		}
		if !supportsFK {
			skipped = append(skipped, fmt.Sprintf("外键 %s 未建：%s 不支持 ALTER TABLE ADD FOREIGN KEY", name, dstEngine))
			continue
		}
		if fk.RefTable != table && exists != nil && !exists(fk.RefTable) {
			skipped = append(skipped, fmt.Sprintf("外键 %s 未建：目标库没有表 %s", name, fk.RefTable))
			continue
		}
		stmts = append(stmts, d.AddForeignKeySQL(quotedTable, name, fk.Columns,
			d.QuoteIdent(fk.RefTable), fk.RefColumns, fk.OnDelete, fk.OnUpdate)+";")
	}
	return stmts, skipped
}

// mysqlMaxTableComment 是 MySQL/MariaDB 的表注释上限（字符数）。超了引擎报 1629
// 而不是截断，而注释撑不住不该让整次迁移失败——超了就说一声跳过。
const mysqlMaxTableComment = 2048

// BuildMigrationCommentDDL 生成注释迁移语句（ADR-0014）。
//
// PostgreSQL 的表与列注释都能搬（COMMENT ON ...）；MySQL 只有**表**注释能一条
// 语句搞定——列注释必须带完整列定义（MODIFY COLUMN ... COMMENT），那要重算类型，
// 价值不抵复杂度，v1 跳过并说明；SQLite 没有注释概念。
//
// 语句由调用方在**数据搬完之后**执行：注释不影响数据，不该挡在数据前面。
func BuildMigrationCommentDDL(dstEngine model.Engine, table string, src *model.TableDetail) (stmts []string, skipped []string) {
	if src == nil {
		return nil, nil
	}
	d := dialect.For(dstEngine)
	quotedTable := d.QuoteIdent(table)
	hasColComment := false
	for _, c := range src.Columns {
		if strings.TrimSpace(c.Comment) != "" {
			hasColComment = true
		}
	}

	switch dstEngine {
	case model.EnginePostgres:
		if s := strings.TrimSpace(src.Comment); s != "" {
			stmts = append(stmts, "COMMENT ON TABLE "+quotedTable+" IS "+quoteLiteral(dstEngine, s)+";")
		}
		for _, c := range src.Columns {
			if s := strings.TrimSpace(c.Comment); s != "" {
				stmts = append(stmts, "COMMENT ON COLUMN "+quotedTable+"."+d.QuoteIdent(c.Name)+" IS "+quoteLiteral(dstEngine, s)+";")
			}
		}
	case model.EngineMySQL, model.EngineMariaDB:
		if s := strings.TrimSpace(src.Comment); s != "" {
			if n := utf8.RuneCountInString(s); n > mysqlMaxTableComment {
				skipped = append(skipped, fmt.Sprintf("表注释未迁移：%d 字符超过 MySQL 上限 %d", n, mysqlMaxTableComment))
			} else {
				stmts = append(stmts, "ALTER TABLE "+quotedTable+" COMMENT = "+quoteLiteral(dstEngine, s)+";")
			}
		}
		if hasColComment {
			skipped = append(skipped, "列注释未迁移：MySQL 需要带完整列定义（MODIFY COLUMN），v1 不重算类型")
		}
	default:
		if hasColComment || strings.TrimSpace(src.Comment) != "" {
			skipped = append(skipped, fmt.Sprintf("注释未迁移：%s 没有注释语法", dstEngine))
		}
	}
	return stmts, skipped
}

// quoteLiteral 把文本写成**目标引擎**的 SQL 单引号字面量。
//
// 只处理单引号是不够的：MySQL 默认 sql_mode 不含 NO_BACKSLASH_ESCAPES，反斜杠在
// 字符串里是转义符，注释里写 C:\temp 会变成制表符前的 C:；PostgreSQL 从 9.1 起
// standard_conforming_strings=on，反斜杠就是普通字符，多双写一个反而多存一个。
// 所以按引擎分开处理——注释被改写属于静默数据失真，和丢数据一样不能接受。
func quoteLiteral(engine model.Engine, s string) string {
	if engine == model.EngineMySQL || engine == model.EngineMariaDB {
		s = strings.ReplaceAll(s, "\\", "\\\\")
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// MaxLenFromTypeText 从类型声明里取出长度上限（"varchar(32)" → 32，"decimal(10,2)" → 10）。
//
// 取不到返回 0：没有声明长度（text / json）就无从比较，调用方必须把 0 当成
// 「不知道」，而不是「上限是 0」。
func MaxLenFromTypeText(s string) int {
	// 解析只有一份（model.TypeArgs）：这里再写一遍，就会出现「决策里写的
	// varchar(32) 认得出、目标表声明的认不出」这种互相矛盾的结论。
	if n, _, ok := model.TypeArgs(s); ok {
		return n
	}
	return 0
}

// ProbeLengths 读源表每列的实际最大字符长度。
//
// 一次全表扫描，代价与表大小成正比——所以它是**显式开启**的（plan -probe），
// 不是每次预检都跑。为什么值得做：按声明长度判「收窄会丢数据」会把一批本来
// 安全的情况拦住（源 varchar(255) 里最长只有 12 个字），用户只能靠猜；而真正
// 危险的反方向是 MySQL 非严格模式下**静默截断**，那必须靠实际值才能提前发现。
//
// 返回空 map 表示没有需要探测的列（都不是字符串列，或都没有声明长度）。
func ProbeLengths(ctx context.Context, src *Executor, table string, cols []string) (map[string]int64, error) {
	if src == nil || len(cols) == 0 {
		return nil, nil
	}
	d := dialect.For(src.conn.Engine())
	exprs := make([]string, len(cols))
	for i, c := range cols {
		exprs[i] = "MAX(" + dialect.CharLengthExpr(src.conn.Engine(), d.QuoteIdent(c)) + ")"
	}
	q := "SELECT " + strings.Join(exprs, ", ") + " FROM " + d.QuoteIdent(table)
	out, err := src.Execute(ctx, Request{SQL: q, Approval: true, Actor: "human:local"})
	if err != nil {
		return nil, fmt.Errorf("探测实际长度: %w", err)
	}
	if out == nil || len(out.Results) == 0 || out.Err != nil {
		return nil, fmt.Errorf("探测实际长度失败: %+v", out)
	}
	rows := out.Results[0].Rows
	if len(rows) != 1 || len(rows[0]) != len(cols) {
		return nil, fmt.Errorf("探测实际长度返回了意外形状: %+v", rows)
	}
	outMap := make(map[string]int64, len(cols))
	for i, c := range cols {
		if n, ok := asInt64(rows[0][i]); ok {
			outMap[c] = n
		}
	}
	return outMap, nil
}

// ApplyProbedLengths 用实际最大长度修正「按声明长度收窄」的阻断结论。
//
// 只动**由长度收窄导致的** blocked：数组类型、无符号溢出这类阻断与数据长度
// 无关，拿实际值去放行它们等于绕过预检。
//
// 放行时把话说全：它证明的是「已有数据放得下」，不是「以后也放得下」。
func ApplyProbedLengths(plan *MigratePlan, decisions map[string]string, actual map[string]int64) {
	if plan == nil {
		return
	}
	for i := range plan.Columns {
		v := &plan.Columns[i]
		n, probed := actual[v.Column]
		if !probed {
			continue
		}
		v.ActualMax = &n
		limit := v.TargetMaxLen
		for k, val := range decisions {
			if strings.EqualFold(k, v.Column) {
				if l := MaxLenFromTypeText(val); l > 0 {
					limit = l
				}
			}
		}
		if limit <= 0 {
			continue
		}
		if int(n) > limit {
			v.Status = MigrateBlocked
			v.Reason = fmt.Sprintf("实际最大 %d 字符超过目标上限 %d：搬到这里会失败或截断", n, limit)
			continue
		}
		// 只有「声明更宽」这一条阻断可以被实际值解开。
		if v.Status == MigrateBlocked && v.SourceMaxLen > limit {
			v.Status = MigrateOK
			v.Reason = fmt.Sprintf("源声明长度 %d 宽于目标上限 %d，但实际最大只有 %d 字符，放行；后续新增写入仍需 ≤ %d",
				v.SourceMaxLen, limit, n, limit)
		}
	}
	recountPlan(plan)
}

// recountPlan 重新统计 blocked / review：调用方会直接读这两个数决定 ready。
func recountPlan(plan *MigratePlan) {
	plan.Blocked, plan.Review = 0, 0
	for _, v := range plan.Columns {
		switch v.Status {
		case MigrateBlocked:
			plan.Blocked++
		case MigrateReview:
			plan.Review++
		}
	}
}

// asInt64 把驱动返回的聚合值收成 int64（空表时 MAX 返回 NULL，调用方按缺失处理）。
func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	case []byte:
		i, err := strconv.ParseInt(string(n), 10, 64)
		return i, err == nil
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		return i, err == nil
	}
	return 0, false
}

// planColumnsToProbe 挑出「实际值能改变结论」的列。
//
// 判据是「有可比较的上限」：源声明了长度，或人工决策/目标表给出了长度上限。
// 两条都没有（text → text）时探了也没得比，白白全表扫一遍。
func planColumnsToProbe(plan *MigratePlan, decisions map[string]string) []string {
	var cols []string
	for _, v := range plan.Columns {
		limit := v.TargetMaxLen
		for k, val := range decisions {
			if strings.EqualFold(k, v.Column) {
				if l := MaxLenFromTypeText(val); l > 0 {
					limit = l
				}
			}
		}
		if v.SourceMaxLen > 0 || limit > 0 {
			cols = append(cols, v.Column)
		}
	}
	return cols
}

// ProbeMigrationLengths 探测实际长度、修正 plan，并按修正后的 plan 重算 DDL。
//
// 之所以连 DDL 一起重算：长度放行会把 blocked 变成 ok，而 blocked 的列在
// BuildMigrationDDL 里是「必须有 decisions」的——不重算的话，调用方拿到的是
// 一份还要求决策的旧结论，与刚印出来的 plan 自相矛盾。
// 第三个返回值表示「DDL 已按新结论重算」：没有可探测的列时它是 false，
// 调用方必须保留原来的 DDL，而不是拿一个空串覆盖掉。
func ProbeMigrationLengths(ctx context.Context, src, dst *Executor, table, targetTable string,
	decisions map[string]string, plan *MigratePlan) (ddl string, recomputed bool, err error) {
	if plan == nil || src == nil {
		return "", false, nil
	}
	cols := planColumnsToProbe(plan, decisions)
	if len(cols) == 0 {
		return "", false, nil
	}
	actual, err := ProbeLengths(ctx, src, table, cols)
	if err != nil {
		return "", false, err
	}
	ApplyProbedLengths(plan, decisions, actual)
	if dst == nil {
		return "", false, nil
	}
	srcDetail, derr := src.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: table})
	if derr != nil {
		return "", false, fmt.Errorf("读源表结构: %w", derr)
	}
	name := targetTable
	if name == "" {
		name = table
	}
	ddl, derr = BuildMigrationDDL(dst.conn.Engine(), name, srcDetail, plan, decisions)
	return ddl, derr == nil, derr
}
