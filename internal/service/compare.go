package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/dialect"
	"github.com/maintell/DBRover/internal/model"
)

// CompareStatus 是一张表在比对中的状态。
type CompareStatus string

const (
	CompareSame       CompareStatus = "same"
	CompareOnlyLeft   CompareStatus = "only_left"
	CompareOnlyRight  CompareStatus = "only_right"
	CompareChanged    CompareStatus = "changed"
	CompareNeedReview CompareStatus = "need_review"
)

// ColumnDiff 描述一列的差异。Left/Right 为 nil 表示该侧没有这一列。
type ColumnDiff struct {
	Name   string        `json:"name"`
	Left   *model.Column `json:"left,omitempty"`
	Right  *model.Column `json:"right,omitempty"`
	Reason []string      `json:"reason,omitempty"`
	Status CompareStatus `json:"status"`
}

// IndexDiff 描述一个索引的差异。
//
// 按「列序列 + 唯一性 + 谓词」为键匹配，不按名字：两边各自起的名字不同
// 不构成功能差异（docs/07 §4.2）。主键索引不参与比对——它的有无已经体现
// 在列的主键标记上，而且删主键不是一条 DROP INDEX 能表达的。
type IndexDiff struct {
	Key    string        `json:"key"`
	Left   *model.Index  `json:"left,omitempty"`
	Right  *model.Index  `json:"right,omitempty"`
	Reason []string      `json:"reason,omitempty"`
	Status CompareStatus `json:"status"`
}

// ForeignKeyDiff 描述一个外键的差异。
//
// 按「本表列 → 引用表 + 引用列」为键匹配，不按名字（名字不同不构成功能差异）。
// 动作子句（ON DELETE / ON UPDATE）参与比较但不参与键：同一个关系改了删除行为
// 应当报成「同一外键有差异」，而不是变成两条互不相干的新增与删除。
type ForeignKeyDiff struct {
	Key    string            `json:"key"`
	Left   *model.ForeignKey `json:"left,omitempty"`
	Right  *model.ForeignKey `json:"right,omitempty"`
	Reason []string          `json:"reason,omitempty"`
	Status CompareStatus     `json:"status"`
}

// CheckDiff 描述一个检查约束的差异。按归一化后的表达式为键。
type CheckDiff struct {
	Key    string        `json:"key"`
	Left   *model.Check  `json:"left,omitempty"`
	Right  *model.Check  `json:"right,omitempty"`
	Reason []string      `json:"reason,omitempty"`
	Status CompareStatus `json:"status"`
}

// TableDiff 描述一张表的差异。
type TableDiff struct {
	Name        string           `json:"name"`
	Status      CompareStatus    `json:"status"`
	Columns     []ColumnDiff     `json:"columns,omitempty"`
	Indexes     []IndexDiff      `json:"indexes,omitempty"`
	ForeignKeys []ForeignKeyDiff `json:"foreign_keys,omitempty"`
	Checks      []CheckDiff      `json:"checks,omitempty"`
	// LeftDDL / RightDDL 是两侧的原始结构原文，供人眼对照；只在状态不是 same 时给出。
	// 两侧都有（状态 changed）时给全，仅在单侧时只给存在的那一侧。
	LeftDDL  string `json:"left_ddl,omitempty"`
	RightDDL string `json:"right_ddl,omitempty"`
}

// ViewDiff 描述一个视图的差异。
//
// 只比「查询体」，不比头部：MySQL 的 SHOW CREATE VIEW 会带上 ALGORITHM / DEFINER /
// SQL SECURITY，PostgreSQL 的 pg_get_viewdef 只给查询、SQLite 原样存原文。
// 这三段头部是部署属性（谁建的、以谁的身份跑），不是结构差异，混进来会让
// 同一段查询在两边永远显示「不一样」。
type ViewDiff struct {
	// Kind 区分普通视图与物化视图：两者不是同一类对象——物化视图会把结果
	// 存下来（删了丢的是已物化的数据），读之前还要 REFRESH。混在一张清单里，
	// 读的人分不清哪条需要刷新。
	Kind model.ObjectKind `json:"kind,omitempty"`
	Name string           `json:"name"`
	// Left / Right 是两侧视图的原始 DDL（取不到时为空）。
	Left   string        `json:"left,omitempty"`
	Right  string        `json:"right,omitempty"`
	Reason []string      `json:"reason,omitempty"`
	Status CompareStatus `json:"status"`
}

// CompareResult 是两侧结构比对的结论。
type CompareResult struct {
	LeftEngine  model.Engine `json:"left_engine"`
	RightEngine model.Engine `json:"right_engine"`
	Tables      []TableDiff  `json:"tables"`
	Views       []ViewDiff   `json:"views,omitempty"`
	// Upgrade 是「把左侧改成右侧」的语句；Downgrade 是反向。
	// v1 只生成「建缺失的表 / 加缺失的列」这类增量语句，删除与改类型一律不自动生成。
	Upgrade   []string `json:"upgrade,omitempty"`
	Downgrade []string `json:"downgrade,omitempty"`
	// NeedReview 是需要人工决策的点（跨引擎类型差异等）。
	NeedReview []string `json:"need_review,omitempty"`
}

// Compare 比对两个连接的结构。
//
// 只做结构，不做数据（那是迁移模块的事）；也不自动执行比对结果——
// 生成的语句只是文本，要落地仍需走变更预案。
func Compare(ctx context.Context, left, right driver.Connector) (*CompareResult, error) {
	leftTables, err := describeAll(ctx, left)
	if err != nil {
		return nil, err
	}
	rightTables, err := describeAll(ctx, right)
	if err != nil {
		return nil, err
	}

	res := &CompareResult{
		LeftEngine:  left.Engine(),
		RightEngine: right.Engine(),
	}
	crossEngine := left.Engine() != right.Engine()
	leftDialect := dialect.For(left.Engine())
	rightDialect := dialect.For(right.Engine())

	names := map[string]bool{}
	for n := range leftTables {
		names[n] = true
	}
	for n := range rightTables {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, name := range sorted {
		lt, lok := leftTables[strings.ToLower(name)]
		rt, rok := rightTables[strings.ToLower(name)]

		switch {
		case lok && !rok:
			res.Tables = append(res.Tables, TableDiff{
				Name: lt.Ref.Name, Status: CompareOnlyLeft,
				LeftDDL: ddlOrEmpty(ctx, left, lt.Ref),
			})
			// 左有右无：升级要删表，但删表不可逆，不自动生成。
			res.NeedReview = append(res.NeedReview,
				lt.Ref.Name+"：左侧存在而右侧不存在，删除表需要人工确认")
		case !lok && rok:
			d := TableDiff{
				Name: rt.Ref.Name, Status: CompareOnlyRight,
				RightDDL: ddlOrEmpty(ctx, right, rt.Ref),
			}
			res.Tables = append(res.Tables, d)
			if d.RightDDL != "" {
				res.Upgrade = append(res.Upgrade, terminate(d.RightDDL))
			}
			// 反向（降级）是删表，同样不自动生成。
			res.NeedReview = append(res.NeedReview,
				rt.Ref.Name+"：右侧新增表，回退脚本需要删表，未自动生成")
		default:
			td := compareTable(lt, rt, crossEngine)
			if td.Status == CompareSame {
				continue
			}
			// 有差异时把两侧原文都带上：差异清单说的是「哪一列不对」，
			// 要判断该怎么改还得看整张表长什么样。
			td.LeftDDL = ddlOrEmpty(ctx, left, lt.Ref)
			td.RightDDL = ddlOrEmpty(ctx, right, rt.Ref)
			res.Tables = append(res.Tables, td)
			appendColumnScripts(&res.Upgrade, &res.Downgrade, &res.NeedReview,
				td, leftDialect, rightDialect)
			appendIndexScripts(&res.Upgrade, &res.Downgrade, &res.NeedReview, td, leftDialect)
			appendForeignKeyScripts(&res.Upgrade, &res.Downgrade, &res.NeedReview,
				td, leftDialect, rightDialect, left.Capabilities().AddForeignKey)
		}
	}

	// 视图与物化视图分开比（见 ViewDiff.Kind 的注释）。
	appendViewDiffs(ctx, res, left, right, crossEngine, leftDialect, model.KindView)
	appendViewDiffs(ctx, res, left, right, crossEngine, leftDialect, model.KindMatView)
	return res, nil
}

// ddlOrEmpty 取对象原文，失败时返回空串。
//
// 比对的主结论不该因为「某一侧取不到 DDL」而整体失败：视图和某些系统对象
// 本来就可能没有原文。取不到就是没有对照材料，差异照样按元数据算。
func ddlOrEmpty(ctx context.Context, c driver.Connector, ref model.ObjectRef) string {
	ddl, err := c.Meta().DDL(ctx, ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(ddl)
}

// terminate 给语句补上结尾分号，已经有就不重复加。
func terminate(sqlText string) string {
	s := strings.TrimSpace(sqlText)
	if strings.HasSuffix(s, ";") {
		return s
	}
	return s + ";"
}

// viewDef 是一侧某个视图的定义。
type viewDef struct {
	ref  model.ObjectRef
	ddl  string // 原始 DDL；取不到时为空
	body string // 归一化后的查询体；ddl 为空时也为空
}

// describeViews 取一侧的全部视图（或物化视图），键为小写名字。
func describeViews(ctx context.Context, c driver.Connector, kind model.ObjectKind) (map[string]viewDef, error) {
	objs, err := c.Meta().Objects(ctx, "", "", kind)
	if err != nil {
		return nil, err
	}
	out := make(map[string]viewDef, len(objs))
	for _, o := range objs {
		d := viewDef{ref: o}
		if ddl := ddlOrEmpty(ctx, c, o); ddl != "" {
			d.ddl = ddl
			d.body = normalizeViewBody(viewQueryBody(ddl))
		}
		out[strings.ToLower(o.Name)] = d
	}
	return out, nil
}

// appendViewDiffs 比对两侧视图定义，把结论直接写进 res。
//
// 与列的取舍不同：删视图不丢数据（定义就在回退脚本里），所以两个方向都生成脚本。
// 跨引擎时视图原文是另一套方言写的，直接放到升级脚本里会执行失败，
// 因此只给原文并记 need_review——这一点与表结构原文的处理保持一致。
func appendViewDiffs(ctx context.Context, res *CompareResult, left, right driver.Connector,
	crossEngine bool, leftDialect dialect.Dialect, kind model.ObjectKind) {

	label := "视图"
	// 删一个物化视图丢的是**已物化的结果**（源数据不动），这句话不能套在视图上，
	// 也不能反过来——两者在预案与提示里的说法都不一样。
	dataNote := "不丢数据"
	if kind == model.KindMatView {
		label = "物化视图"
		dataNote = "只丢已物化的结果，源数据不动"
	}
	lv, lerr := describeViews(ctx, left, kind)
	rv, rerr := describeViews(ctx, right, kind)
	if lerr != nil || rerr != nil {
		which, err := "左侧", lerr
		if lerr == nil {
			which, err = "右侧", rerr
		}
		res.NeedReview = append(res.NeedReview, which+label+"清单读取失败："+err.Error())
		return
	}
	// 一侧引擎根本没有这类对象（能力位为假）时，差异不是「有人忘了建」，而是
	// 「这个引擎没有这个概念」：不生成脚本，只记一条需要人工决策。
	supportsLeft := kind == model.KindView || left.Capabilities().MaterializedViews
	supportsRight := kind == model.KindView || right.Capabilities().MaterializedViews

	names := map[string]bool{}
	for n := range lv {
		names[n] = true
	}
	for n := range rv {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, name := range sorted {
		l, lok := lv[name]
		r, rok := rv[name]

		switch {
		case lok && !rok:
			if !supportsRight {
				res.NeedReview = append(res.NeedReview,
					l.ref.Name+"：右侧引擎没有"+label+"，左侧这个对象怎么处理需要人工决定（未生成脚本）")
				continue
			}
			d := ViewDiff{Kind: kind, Name: l.ref.Name, Left: l.ddl, Status: CompareOnlyLeft}
			res.Views = append(res.Views, d)
			res.Upgrade = append(res.Upgrade, viewDropSQL(leftDialect, kind, leftDialect.QuoteQualified("", l.ref.Name))+";")
			if l.ddl != "" {
				res.Downgrade = append(res.Downgrade, terminate(l.ddl))
			} else {
				res.NeedReview = append(res.NeedReview,
					l.ref.Name+"：左侧"+label+"取不到定义，回退脚本无法生成")
			}
			res.NeedReview = append(res.NeedReview,
				l.ref.Name+"：左侧有而右侧没有"+label+"，升级脚本会删掉它（"+dataNote+"，定义在回退脚本里）")

		case !lok && rok:
			if !supportsLeft {
				res.NeedReview = append(res.NeedReview,
					r.ref.Name+"：左侧引擎没有"+label+"，右侧这个对象在升级后无处安放（未生成脚本）")
				continue
			}
			d := ViewDiff{Kind: kind, Name: r.ref.Name, Right: r.ddl, Status: CompareOnlyRight}
			res.Views = append(res.Views, d)
			if crossEngine {
				res.NeedReview = append(res.NeedReview,
					r.ref.Name+"：右侧有而左侧没有"+label+"，但两侧引擎不同，"+
						"右侧的定义原文不一定能在左侧执行，需人工改写")
			} else if r.ddl != "" {
				res.Upgrade = append(res.Upgrade, terminate(r.ddl))
			} else {
				res.NeedReview = append(res.NeedReview,
					r.ref.Name+"：右侧"+label+"取不到定义，升级脚本无法生成")
			}
			res.Downgrade = append(res.Downgrade,
				viewDropSQL(leftDialect, kind, leftDialect.QuoteQualified("", r.ref.Name))+";")

		default:
			reasons := viewReasons(l, r)
			if len(reasons) == 0 {
				continue
			}
			res.Views = append(res.Views, ViewDiff{
				Kind: kind, Name: l.ref.Name, Left: l.ddl, Right: r.ddl,
				Reason: reasons, Status: CompareChanged,
			})
			// 改视图要 CREATE OR REPLACE（SQLite 没有这个写法，只能 DROP + CREATE），
			// 两侧引擎支持程度不一，不在这里假装能生成。
			hint := "改视图需人工确认后手写 DDL"
			if kind == model.KindMatView {
				hint = "物化视图的定义不能就地改，只能 DROP 后重建；重建会重算一遍并覆盖已物化的数据"
			}
			res.NeedReview = append(res.NeedReview,
				l.ref.Name+"："+label+"定义有差异（"+strings.Join(reasons, "；")+"），"+hint)
		}
	}
}

// viewDropSQL 渲染删对象语句：视图与物化视图不是同一种 DROP。
//
// 用错会直接语法错——`DROP MATERIALIZED VIEW` 在 PG 上是另一条路径，
// 而 `DROP VIEW` 删不动物化视图。
func viewDropSQL(d dialect.Dialect, kind model.ObjectKind, name string) string {
	if kind == model.KindMatView {
		return d.DropMaterializedViewSQL(name)
	}
	return d.DropViewSQL(name)
}

// viewReasons 判断同一视图在两侧是否等价。
//
// 任一取不到定义时不下「相同」的结论——把「不知道」判成相同，比报差异更危险。
func viewReasons(l, r viewDef) []string {
	if l.body == "" || r.body == "" {
		side := "左侧"
		if l.body != "" {
			side = "右侧"
		}
		return []string{side + "取不到视图定义，无法比对查询体"}
	}
	if l.body == r.body {
		return nil
	}
	return []string{"查询体不同"}
}

// viewQueryBody 取出 CREATE VIEW 语句里的查询体。
//
// 不用正则找 " AS "：AS 也可能出现在字符串字面量或括号里的列别名中。
// 这里的判据是「第一个位于括号外层、不在引号里的 AS 」——也就是
// CREATE ... VIEW <名字> [(列清单)] AS <查询> 的那个分界。
func viewQueryBody(ddl string) string {
	inQuote := byte(0)
	depth := 0
	for i := 0; i < len(ddl); i++ {
		c := ddl[i]
		if inQuote != 0 {
			if c == inQuote {
				// 两个相同引号连写是转义（SQL 标准），成对跳过
				if i+1 < len(ddl) && ddl[i+1] == inQuote {
					i++
					continue
				}
				inQuote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			inQuote = c
		case '[':
			inQuote = ']'
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case 'A', 'a':
			if depth == 0 && keywordAt(ddl, i, "AS") {
				return strings.TrimSpace(ddl[i+2:])
			}
		}
	}
	return strings.TrimSpace(ddl)
}

// keywordAt 判断 s[i:] 是否以关键字 word 开头，且两侧都是标识符边界。
func keywordAt(s string, i int, word string) bool {
	if i+len(word) > len(s) {
		return false
	}
	if !strings.EqualFold(s[i:i+len(word)], word) {
		return false
	}
	if i > 0 && isIdentByte(s[i-1]) {
		return false
	}
	if i+len(word) < len(s) && isIdentByte(s[i+len(word)]) {
		return false
	}
	return true
}

func isIdentByte(c byte) bool {
	return c == '_' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		c >= 0x80 // 非 ASCII 也可能是标识符字符
}

// normalizeViewBody 抹平纯写法差异：折叠空白、去掉结尾分号。
//
// 刻意不做的两件事：不去标识符引号、不改大小写。把 "A" 与 "a" 判成相同
// 只在大小写不敏感的引擎上成立，而字符串字面量处处大小写敏感——
// 误报一次可以人工核对，误判「相同」会把真的差异藏起来。
func normalizeViewBody(s string) string {
	out := strings.Join(strings.Fields(s), " ")
	return strings.TrimSuffix(strings.TrimSpace(out), ";")
}

// describeAll 取一侧的全部表结构，键为小写表名。
func describeAll(ctx context.Context, c driver.Connector) (map[string]*model.TableDetail, error) {
	objs, err := c.Meta().Objects(ctx, "", "", model.KindTable)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*model.TableDetail, len(objs))
	for _, o := range objs {
		td, err := c.Meta().DescribeTable(ctx, o)
		if err != nil {
			// 视图或没有列的对象的描述可能失败；比对不该因此整体失败。
			continue
		}
		out[strings.ToLower(o.Name)] = td
	}
	return out, nil
}

// compareTable 逐列比对两张表。
func compareTable(left, right *model.TableDetail, crossEngine bool) TableDiff {
	td := TableDiff{Name: left.Ref.Name}

	leftCols := make(map[string]model.Column, len(left.Columns))
	for _, c := range left.Columns {
		leftCols[strings.ToLower(c.Name)] = c
	}
	rightCols := make(map[string]model.Column, len(right.Columns))
	for _, c := range right.Columns {
		rightCols[strings.ToLower(c.Name)] = c
	}

	names := map[string]bool{}
	for n := range leftCols {
		names[n] = true
	}
	for n := range rightCols {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, key := range sorted {
		lc, lok := leftCols[key]
		rc, rok := rightCols[key]

		switch {
		case lok && !rok:
			l := lc
			td.Columns = append(td.Columns, ColumnDiff{Name: lc.Name, Left: &l, Status: CompareOnlyLeft})
		case !lok && rok:
			r := rc
			td.Columns = append(td.Columns, ColumnDiff{Name: rc.Name, Right: &r, Status: CompareOnlyRight})
		default:
			l, r := lc, rc
			reasons := columnReasons(lc, rc, crossEngine)
			if len(reasons) == 0 {
				continue
			}
			status := CompareChanged
			if crossEngine && hasTypeReason(reasons) {
				// 跨引擎的类型差异一律交给人判断，不自动映射。
				status = CompareNeedReview
			}
			td.Columns = append(td.Columns, ColumnDiff{
				Name: lc.Name, Left: &l, Right: &r, Reason: reasons, Status: status,
			})
		}
	}

	td.Indexes = compareIndexes(left, right)
	td.ForeignKeys = compareForeignKeys(left, right)
	td.Checks = compareChecks(left, right)

	if len(td.Columns) == 0 && len(td.Indexes) == 0 && len(td.ForeignKeys) == 0 && len(td.Checks) == 0 {
		td.Status = CompareSame
		return td
	}
	td.Status = CompareSame
	for _, c := range td.Columns {
		if c.Status != CompareSame {
			td.Status = CompareChanged
		}
		if c.Status == CompareNeedReview {
			td.Status = CompareNeedReview
		}
	}
	for _, ix := range td.Indexes {
		if ix.Status != CompareSame {
			td.Status = CompareChanged
		}
	}
	for _, fk := range td.ForeignKeys {
		if fk.Status != CompareSame {
			td.Status = CompareChanged
		}
		if fk.Status == CompareNeedReview {
			td.Status = CompareNeedReview
		}
	}
	for _, ck := range td.Checks {
		if ck.Status != CompareSame {
			td.Status = CompareChanged
		}
		if ck.Status == CompareNeedReview {
			td.Status = CompareNeedReview
		}
	}
	return td
}

// diffByKey 是「按 key 对齐两份集合」的通用骨架。
//
// 索引、外键、检查约束的比对骨架完全一样，只有「同一个 key 下怎么判差异」不同。
// 写成一个泛型函数而不是复制三遍：三份几乎相同的循环最容易在改一处时漏掉另外两处。
func diffByKey[V any, D any](
	left, right map[string]V,
	same func(l, r V) ([]string, CompareStatus),
	leftOnly func(k string, l V) D,
	rightOnly func(k string, r V) D,
	changed func(k string, l, r V, reasons []string, st CompareStatus) D,
) []D {

	seen := make(map[string]bool, len(left)+len(right))
	keys := make([]string, 0, len(left)+len(right))
	for k := range left {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range right {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var out []D
	for _, k := range keys {
		l, lok := left[k]
		r, rok := right[k]
		switch {
		case lok && !rok:
			out = append(out, leftOnly(k, l))
		case !lok && rok:
			out = append(out, rightOnly(k, r))
		default:
			reasons, st := same(l, r)
			if st == CompareSame && len(reasons) == 0 {
				continue
			}
			out = append(out, changed(k, l, r, reasons, st))
		}
	}
	return out
}

// fkKey 是外键的比对键：本表列 → 引用表 + 引用列。
// 动作子句不进键（同一关系改了删除行为是「有差异」，不是「一删一增」）。
func fkKey(fk model.ForeignKey) string {
	cols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		cols[i] = strings.ToLower(strings.TrimSpace(c))
	}
	refs := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		refs[i] = strings.ToLower(strings.TrimSpace(c))
	}
	return strings.Join(cols, ",") + "->" +
		strings.ToLower(strings.TrimSpace(fk.RefTable)) + "(" + strings.Join(refs, ",") + ")"
}

// compareForeignKeys 对齐两侧外键。
func compareForeignKeys(left, right *model.TableDetail) []ForeignKeyDiff {
	collect := func(td *model.TableDetail) map[string]model.ForeignKey {
		out := make(map[string]model.ForeignKey, len(td.ForeignKeys))
		for _, fk := range td.ForeignKeys {
			out[fkKey(fk)] = fk
		}
		return out
	}
	return diffByKey(collect(left), collect(right),
		func(l, r model.ForeignKey) ([]string, CompareStatus) {
			var reasons []string
			if !strings.EqualFold(strings.TrimSpace(l.OnDelete), strings.TrimSpace(r.OnDelete)) {
				reasons = append(reasons, "ON DELETE 不同: "+actionOrNone(l.OnDelete)+" → "+actionOrNone(r.OnDelete))
			}
			if !strings.EqualFold(strings.TrimSpace(l.OnUpdate), strings.TrimSpace(r.OnUpdate)) {
				reasons = append(reasons, "ON UPDATE 不同: "+actionOrNone(l.OnUpdate)+" → "+actionOrNone(r.OnUpdate))
			}
			if len(reasons) > 0 {
				return reasons, CompareChanged
			}
			// 名字不同不影响语义，但要说出来，免得人以为漏了。
			if !strings.EqualFold(l.Name, r.Name) {
				return []string{"外键名不同: " + l.Name + " → " + r.Name}, CompareChanged
			}
			return nil, CompareSame
		},
		func(k string, l model.ForeignKey) ForeignKeyDiff {
			v := l
			return ForeignKeyDiff{Key: k, Left: &v, Status: CompareOnlyLeft}
		},
		func(k string, r model.ForeignKey) ForeignKeyDiff {
			v := r
			return ForeignKeyDiff{Key: k, Right: &v, Status: CompareOnlyRight}
		},
		func(k string, l, r model.ForeignKey, reasons []string, st CompareStatus) ForeignKeyDiff {
			lv, rv := l, r
			return ForeignKeyDiff{Key: k, Left: &lv, Right: &rv, Reason: reasons, Status: st}
		},
	)
}

func actionOrNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "NO ACTION"
	}
	return strings.ToUpper(strings.TrimSpace(s))
}

// checkKey 是检查约束的比对键：归一化后的表达式（名字不同不算差异）。
func checkKey(c model.Check) string {
	return normalizeTypeText(c.Expr)
}

// compareChecks 对齐两侧检查约束。
func compareChecks(left, right *model.TableDetail) []CheckDiff {
	collect := func(td *model.TableDetail) map[string]model.Check {
		out := make(map[string]model.Check, len(td.Checks))
		for _, c := range td.Checks {
			out[checkKey(c)] = c
		}
		return out
	}
	return diffByKey(collect(left), collect(right),
		func(l, r model.Check) ([]string, CompareStatus) {
			if !strings.EqualFold(strings.TrimSpace(l.Name), strings.TrimSpace(r.Name)) {
				return []string{"约束名不同: " + l.Name + " → " + r.Name}, CompareChanged
			}
			return nil, CompareSame
		},
		func(k string, l model.Check) CheckDiff {
			v := l
			return CheckDiff{Key: k, Left: &v, Status: CompareOnlyLeft}
		},
		func(k string, r model.Check) CheckDiff {
			v := r
			return CheckDiff{Key: k, Right: &v, Status: CompareOnlyRight}
		},
		func(k string, l, r model.Check, reasons []string, st CompareStatus) CheckDiff {
			lv, rv := l, r
			return CheckDiff{Key: k, Left: &lv, Right: &rv, Reason: reasons, Status: st}
		},
	)
}

// indexKey 是索引的比对键：唯一性 + 谓词 + 列序列。
func indexKey(ix model.Index) string {
	cols := make([]string, len(ix.Columns))
	for i, c := range ix.Columns {
		cols[i] = strings.ToLower(strings.TrimSpace(c))
	}
	return fmt.Sprintf("%t|%s|%s",
		ix.Unique, strings.ToLower(strings.TrimSpace(ix.Predicate)), strings.Join(cols, ","))
}

// compareIndexes 按比对键对齐两侧索引。
func compareIndexes(left, right *model.TableDetail) []IndexDiff {
	collect := func(td *model.TableDetail) map[string]model.Index {
		out := make(map[string]model.Index, len(td.Indexes))
		for _, ix := range td.Indexes {
			if ix.Primary {
				continue // 主键由列上的标记体现，不当作普通索引处理
			}
			out[indexKey(ix)] = ix
		}
		return out
	}
	leftIdx := collect(left)
	rightIdx := collect(right)

	seen := map[string]bool{}
	keys := make([]string, 0, len(leftIdx)+len(rightIdx))
	for k := range leftIdx {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range rightIdx {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var out []IndexDiff
	for _, k := range keys {
		li, lok := leftIdx[k]
		ri, rok := rightIdx[k]

		switch {
		case lok && !rok:
			l := li
			out = append(out, IndexDiff{Key: k, Left: &l, Status: CompareOnlyLeft})
		case !lok && rok:
			r := ri
			out = append(out, IndexDiff{Key: k, Right: &r, Status: CompareOnlyRight})
		default:
			// 键相同说明列序列、唯一性、谓词都一致；名字或访问方法不同只是形式差异。
			l, r := li, ri
			var reasons []string
			if l.Name != r.Name {
				reasons = append(reasons, "索引名不同: "+l.Name+" → "+r.Name)
			}
			if !strings.EqualFold(strings.TrimSpace(l.Method), strings.TrimSpace(r.Method)) {
				reasons = append(reasons, "索引方法不同: "+l.Method+" → "+r.Method)
			}
			if len(reasons) == 0 {
				continue
			}
			out = append(out, IndexDiff{Key: k, Left: &l, Right: &r, Reason: reasons, Status: CompareChanged})
		}
	}
	return out
}

// columnReasons 列出同一列在两侧的差异原因。
func columnReasons(left, right model.Column, crossEngine bool) []string {
	var out []string

	if left.Type.Kind != right.Type.Kind {
		out = append(out, "中立类型不同: "+string(left.Type.Kind)+" → "+string(right.Type.Kind))
	} else if normalizeTypeText(left.Type.Raw) != normalizeTypeText(right.Type.Raw) {
		// 中立类型相同但原始声明不同（varchar(50) vs varchar(100)）也是差异。
		out = append(out, "原始类型不同: "+left.Type.Raw+" → "+right.Type.Raw)
	}
	if left.Nullable != right.Nullable {
		out = append(out, "可空不同: "+boolText(left.Nullable)+" → "+boolText(right.Nullable))
	}
	if left.IsPrimary != right.IsPrimary {
		out = append(out, "主键标记不同: "+yesNo(left.IsPrimary)+" → "+yesNo(right.IsPrimary))
	}
	if left.IsUnique != right.IsUnique {
		out = append(out, "唯一标记不同: "+yesNo(left.IsUnique)+" → "+yesNo(right.IsUnique))
	}
	if !sameDefault(left.Default, right.Default) {
		out = append(out, "默认值不同: "+defaultText(left.Default)+" → "+defaultText(right.Default))
	}
	if strings.TrimSpace(left.Comment) != strings.TrimSpace(right.Comment) {
		out = append(out, "注释不同")
	}
	if crossEngine && len(out) > 0 {
		out = append(out, "跨引擎比对：类型差异需人工决策")
	}
	return out
}

func hasTypeReason(reasons []string) bool {
	for _, r := range reasons {
		if strings.HasPrefix(r, "中立类型不同") || strings.HasPrefix(r, "原始类型不同") {
			return true
		}
	}
	return false
}

// normalizeTypeText 归一化原始类型文本，减少纯写法差异带来的假阳性。
//
// 做两件事实上的清理，而不是「聪明的类型映射」：
//   - 同义写法归一：PostgreSQL 自己把 varchar 打印成 character varying、
//     把 int 打印成 integer，它们是同一个类型，不归一就会每次跨引擎比对都报差异。
//   - 剥掉 MySQL 的整数显示宽度（bigint(20)、int(11)）：MySQL 8 已废弃该语法。
//     decimal(10,2) 这类带两位数字的括号必须保留——那是精度，不是显示宽度。
func normalizeTypeText(s string) string {
	out := strings.NewReplacer(
		"character varying", "varchar",
		"integer", "int",
	).Replace(strings.ToLower(strings.Join(strings.Fields(s), " ")))

	open := strings.IndexByte(out, '(')
	if open < 0 || !strings.HasSuffix(out, ")") {
		return out
	}
	inner := out[open+1 : len(out)-1]
	switch out[:open] {
	case "tinyint", "smallint", "mediumint", "int", "bigint":
		if _, err := strconv.Atoi(inner); err == nil {
			return out[:open]
		}
	}
	return out
}

func boolText(b bool) string {
	if b {
		return "可空"
	}
	return "非空"
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

// appendForeignKeyScripts 生成外键与检查约束的差异结论。
//
// 外键：右侧有的生成加约束语句，左侧多的生成删约束语句（两个方向都给，因为
// 摘约束不动数据）；检查约束目前没有 DDL 动作，一律进 need_review。
//
// 加外键虽然能生成语句，但它在左侧能不能落地还取决于「父表存在、子表无孤儿」，
// 这两件事比对本身不查，所以额外给一条可执行的核对路径（/fk/precheck）。
func appendForeignKeyScripts(upgrade, downgrade, needReview *[]string, td TableDiff,
	leftDialect, rightDialect dialect.Dialect, leftCanAddFK bool) {

	table := leftDialect.QuoteQualified("", td.Name)
	refTable := func(fk *model.ForeignKey) string {
		// 引用表也要按左侧方言引用；schema 由 quoteQualified 的第二个参数给空，
		// 与列脚本保持一致（跨 schema 场景请人工确认）。
		return leftDialect.QuoteQualified("", fk.RefTable)
	}

	for _, fk := range td.ForeignKeys {
		switch fk.Status {
		case CompareOnlyRight:
			// 左侧引擎不支持加约束时不能给一条必然失败的语句（SQLite 只能重建表）。
			if !leftCanAddFK {
				*needReview = append(*needReview,
					td.Name+"：右侧有外键 "+fk.Key+"，但左侧引擎不支持用 ALTER TABLE 加约束，需重建表或手写迁移")
				continue
			}
			name := fk.Right.Name
			if name == "" {
				name = "fk_" + td.Name + "_" + strings.Join(fk.Right.Columns, "_")
			}
			*upgrade = append(*upgrade,
				leftDialect.AddForeignKeySQL(table, name, fk.Right.Columns,
					refTable(fk.Right), fk.Right.RefColumns, fk.Right.OnDelete, fk.Right.OnUpdate)+";")
			*downgrade = append(*downgrade, leftDialect.DropForeignKeySQL(table, name)+";")
			*needReview = append(*needReview,
				td.Name+"："+fk.Key+" 加外键前先确认父表存在且子表没有孤儿数据（可用 /fk/precheck 预检）")
		case CompareOnlyLeft:
			*upgrade = append(*upgrade, leftDialect.DropForeignKeySQL(table, fk.Left.Name)+";")
			if leftCanAddFK {
				*downgrade = append(*downgrade,
					leftDialect.AddForeignKeySQL(table, fk.Left.Name, fk.Left.Columns,
						refTable(fk.Left), fk.Left.RefColumns, fk.Left.OnDelete, fk.Left.OnUpdate)+";")
			}
		default:
			*needReview = append(*needReview,
				td.Name+"."+fk.Key+"：外键定义不同（"+strings.Join(fk.Reason, "；")+"), "+
					"改外键动作需人工决策")
		}
	}

	for _, ck := range td.Checks {
		switch ck.Status {
		case CompareOnlyRight:
			*needReview = append(*needReview,
				td.Name+"：右侧多了检查约束 "+ck.Key+"，本工具还没有加检查约束的动作，需手写 DDL")
		case CompareOnlyLeft:
			*needReview = append(*needReview,
				td.Name+"：左侧多出的检查约束 "+ck.Key+"，删除前请确认没有依赖它拦数据")
		default:
			*needReview = append(*needReview,
				td.Name+"."+ck.Key+"：检查约束定义不同（"+strings.Join(ck.Reason, "；")+"）")
		}
	}
}

// appendIndexScripts 生成索引的升级与回退语句。
//
// 与列不同，删索引不丢数据，只丢性能，所以这里两个方向都生成脚本：
// 对齐索引是可逆的，没必要推给人。脚本都在**左侧**执行，因此一律用左侧方言
// 渲染引用——跨引擎比对时右侧的类型名才需要保留右侧的写法。
func appendIndexScripts(upgrade, downgrade, needReview *[]string, td TableDiff, leftDialect dialect.Dialect) {
	table := leftDialect.QuoteQualified("", td.Name)

	for _, ix := range td.Indexes {
		switch ix.Status {
		case CompareOnlyRight:
			*upgrade = append(*upgrade,
				leftDialect.CreateIndexSQL(table, ix.Right.Name, ix.Right.Columns, ix.Right.Unique)+";")
			*downgrade = append(*downgrade,
				leftDialect.DropIndexSQL(table, ix.Right.Name)+";")
		case CompareOnlyLeft:
			*upgrade = append(*upgrade,
				leftDialect.DropIndexSQL(table, ix.Left.Name)+";")
			*downgrade = append(*downgrade,
				leftDialect.CreateIndexSQL(table, ix.Left.Name, ix.Left.Columns, ix.Left.Unique)+";")
		default:
			*needReview = append(*needReview,
				td.Name+"."+ix.Key+"：索引定义不同（"+strings.Join(ix.Reason, "；")+"），"+
					"改名或换访问方法不影响查询结果，未自动生成")
		}
	}
}

// sameDefault 比较默认值，去掉外层括号与大小写差异。
func sameDefault(a, b *string) bool {
	norm := func(p *string) string {
		if p == nil {
			return ""
		}
		s := strings.TrimSpace(*p)
		for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") && len(s) > 2 {
			s = strings.TrimSpace(s[1 : len(s)-1])
		}
		return strings.ToLower(s)
	}
	return norm(a) == norm(b)
}

func defaultText(p *string) string {
	if p == nil {
		return "（无）"
	}
	return *p
}

// appendColumnScripts 把「右侧有、左侧没有」的列转成升级语句。
//
// 只生成增量：加列可逆且安全；删列与改类型要么丢数据要么需要重建表，
// 一律推给人工（记录进 NeedReview），不在这里假装能自动生成。
func appendColumnScripts(upgrade, downgrade, needReview *[]string, td TableDiff,
	leftDialect, rightDialect dialect.Dialect) {

	tableLeft := leftDialect.QuoteQualified("", td.Name)

	for _, c := range td.Columns {
		switch c.Status {
		case CompareOnlyRight:
			def := rightDialect.QuoteIdent(c.Right.Name) + " " + c.Right.Type.Raw
			if !c.Right.Nullable {
				def += " NOT NULL"
			}
			if c.Right.Default != nil {
				def += " DEFAULT " + *c.Right.Default
			}
			*upgrade = append(*upgrade, "ALTER TABLE "+tableLeft+" ADD COLUMN "+def+";")
			// 回退是删列：会丢数据，不自动生成。
			*needReview = append(*needReview,
				td.Name+"."+c.Right.Name+"：左侧缺列，回退需要删列并会丢数据，未自动生成")
		case CompareOnlyLeft:
			*needReview = append(*needReview,
				td.Name+"."+c.Left.Name+"：左侧多出的列，删除会丢数据，未自动生成")
		default:
			*needReview = append(*needReview,
				td.Name+"."+c.Name+"：两侧定义不同（"+strings.Join(c.Reason, "；")+"），改列类型/可空/默认值需人工决策")
		}
	}
}
