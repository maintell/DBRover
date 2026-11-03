package service

import (
	"context"
	"fmt"
	"sort"
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

// TableDiff 描述一张表的差异。
type TableDiff struct {
	Name    string        `json:"name"`
	Status  CompareStatus `json:"status"`
	Columns []ColumnDiff  `json:"columns,omitempty"`
	Indexes []IndexDiff   `json:"indexes,omitempty"`
	// LeftDDL / RightDDL 供人眼对照；只在状态不是 same 时给出。
	LeftDDL  string `json:"left_ddl,omitempty"`
	RightDDL string `json:"right_ddl,omitempty"`
}

// CompareResult 是两侧结构比对的结论。
type CompareResult struct {
	LeftEngine  model.Engine `json:"left_engine"`
	RightEngine model.Engine `json:"right_engine"`
	Tables      []TableDiff  `json:"tables"`
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
			res.Tables = append(res.Tables, TableDiff{Name: lt.Ref.Name, Status: CompareOnlyLeft, LeftDDL: ""})
			// 左有右无：升级要删表，但删表不可逆，不自动生成。
			res.NeedReview = append(res.NeedReview,
				lt.Ref.Name+"：左侧存在而右侧不存在，删除表需要人工确认")
		case !lok && rok:
			d := TableDiff{Name: rt.Ref.Name, Status: CompareOnlyRight}
			res.Tables = append(res.Tables, d)
			if ddl, err := right.Meta().DDL(ctx, rt.Ref); err == nil {
				res.Upgrade = append(res.Upgrade, ddl+";")
			}
			// 反向（降级）是删表，同样不自动生成。
			res.NeedReview = append(res.NeedReview,
				rt.Ref.Name+"：右侧新增表，回退脚本需要删表，未自动生成")
		default:
			td := compareTable(lt, rt, crossEngine)
			if td.Status == CompareSame {
				continue
			}
			res.Tables = append(res.Tables, td)
			appendColumnScripts(&res.Upgrade, &res.Downgrade, &res.NeedReview,
				td, leftDialect, rightDialect)
			appendIndexScripts(&res.Upgrade, &res.Downgrade, &res.NeedReview, td, leftDialect)
		}
	}
	return res, nil
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

	if len(td.Columns) == 0 && len(td.Indexes) == 0 {
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
	return td
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

func normalizeTypeText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
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
