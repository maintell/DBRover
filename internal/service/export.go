package service

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
)

// ExportFormat 是导出格式。
type ExportFormat string

const (
	// ExportCSV 是带引号转义的 CSV，适合直接给表格软件。
	ExportCSV ExportFormat = "csv"
	// ExportNDJSON 是每行一个 JSON 对象，适合流式消费。
	ExportNDJSON ExportFormat = "ndjson"
	// ExportSQL 是「建表 + INSERT」的可回放脚本，用于把一张表搬到另一个库。
	//
	// 只保证**同引擎**回放：字面量按方言生成（二进制 X'..' / '\x..'::bytea、
	// 布尔 TRUE/FALSE 与 1/0），跨引擎请先核对类型映射。
	ExportSQL ExportFormat = "sql"
	// ExportMarkdown 是 GitHub 风格的表：粘到 issue / PR / 文档里就能看。
	//
	// 与另外三种不同，它一定是「给人看的」：表头必写（没有表头的 markdown
	// 表格是坏语法），NULL 写成 NULL、空串写成 ''——与界面上的渲染一致，
	// 因为这张表最常见的用途就是把「界面上看到的东西」贴出去对账。
	ExportMarkdown ExportFormat = "markdown"
	// ExportXLSX 是真正的 Excel 工作簿（xlsx）：表头 + 单元格类型。
	//
	// 与 CSV 的区别不只是「能双击打开」：数字保持数字（不会被 Excel 当文本左对齐），
	// NULL 与空串在 xlsx 里同样都是空单元格——这点与 CSV 一致，要区分请用 ndjson。
	ExportXLSX ExportFormat = "xlsx"
)

// Valid 判断格式是否受支持。
func (f ExportFormat) Valid() bool {
	return f == ExportCSV || f == ExportNDJSON || f == ExportSQL || f == ExportMarkdown ||
		f == ExportXLSX
}

// ErrExportFormat 表示不支持的导出格式。
var ErrExportFormat = fmt.Errorf("不支持的导出格式")

// ExportRequest 是一次导出请求。
type ExportRequest struct {
	Table  string
	Format ExportFormat
	Filter []FilterCond
	Order  *OrderSpec
	// Header 只在 CSV 下有意义。
	Header bool
	// PageSize 是每次从游标取多少行。它是内存上限的唯一来源：
	// 导出永远不把整表读进内存，而不是「先加载再写文件」。
	PageSize int
	// SchemaOnly 只要结构（建表 + 索引），不读任何行。
	//
	// 只对 sql 格式有意义：其它格式没有「结构」这个概念。它的用途是
	// `backup -type schema`（建空库、结构比对），所以必须与 sql 导出**共用同一段**
	// DDL 生成逻辑——另写一份迟早与导出分叉（索引漏补、引号风格不同）。
	SchemaOnly bool
}

// writeSQLPreamble 写 sql 导出的前导：说明注释 + 建表语句 + 该方言需要单补的索引。
//
// 抽出来是因为有两条路径要用它：正常导出（结构 + 数据）与只要结构的导出。
// 复制一份的话，SQLite 的独立索引这种「容易漏」的细节迟早只在一处被修。
func (e *Executor) writeSQLPreamble(ctx context.Context, bw *bufio.Writer, table string, td *model.TableDetail) error {
	fmt.Fprintf(bw, "-- DBRover 导出：%s（%s）\n-- 同引擎回放；跨引擎请先核对类型映射。\n",
		table, e.conn.Engine())
	// 生成列必须说出来：它们在文件里既没有列定义也没有值，读者会以为导出漏了。
	for _, c := range td.Columns {
		if c.IsGenerated {
			fmt.Fprintf(bw, "-- 生成列 %s 不写入（由目标端按表达式计算）\n", c.Name)
		}
	}
	bw.WriteString("\n")
	ddl, err := e.TableDDL(ctx, model.ObjectRef{Kind: model.KindTable, Name: table})
	if err != nil {
		return fmt.Errorf("导出需要建表语句：%w", err)
	}
	bw.WriteString(strings.TrimRight(ddl, "; \n") + ";\n")
	// SQLite 的建表语句不含独立索引，要单独补；漏了就等于静默丢掉唯一性与
	// 查询性能。MySQL / PG 的 DDL 已经带上索引，再补会撞名（真机验证过）。
	// 约束背后的索引一律跳过：它随建表语句一起重建。
	if !e.dialect.DDLIncludesIndexes() {
		for _, ix := range td.Indexes {
			if ix.Primary || ix.Constraint || len(ix.Columns) == 0 {
				continue
			}
			bw.WriteString(e.dialect.CreateIndexSQL(quotePath(e, table), ix.Name, ix.Columns, ix.Unique) + ";\n")
		}
	}
	bw.WriteString("\n")
	return nil
}

// ExportResult 是导出统计。
type ExportResult struct {
	Rows     int64
	Columns  int
	Duration time.Duration
}

// Export 流式导出表数据。
//
// 三点与「先查全部再写」不同：
//   - 单条 SELECT 走游标分批取，内存上限由 PageSize 决定；
//   - 不需要主键：视图与无主键表照样能导（浏览才需要键集分页）；
//   - 列清单显式生成，不用 SELECT *，宽表不会传无用列。
func (e *Executor) Export(ctx context.Context, req ExportRequest, w io.Writer) (*ExportResult, error) {
	started := time.Now()
	format := req.Format
	if format == "" {
		format = ExportCSV
	}
	if !format.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrExportFormat, req.Format)
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = 500
	}
	if pageSize > 1000 {
		pageSize = 1000
	}

	td, err := e.conn.Meta().DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: req.Table})
	if err != nil {
		return nil, err
	}
	known := columnCatalog(td)

	cols := make([]string, len(td.Columns))
	names := make([]string, len(td.Columns))
	// insertIdx 是「可以写入的列」在结果集里的下标：生成列由目标端计算，
	// 写进去引擎会直接报错（SQLite/MySQL/PG 都不接受 INSERT 计算列）。
	// 读路径仍然包含它们——导出成 CSV/JSON 时用户要看得到这一列。
	insertIdx := make([]int, 0, len(td.Columns))
	for i, c := range td.Columns {
		cols[i] = e.dialect.QuoteIdent(c.Name)
		names[i] = c.Name
		if !c.IsGenerated {
			insertIdx = append(insertIdx, i)
		}
	}

	where, args, err := e.filterClause(known, req.Filter)
	if err != nil {
		return nil, err
	}
	order, err := e.orderClause(known, req.Order)
	if err != nil {
		return nil, err
	}

	q := "SELECT " + strings.Join(cols, ", ") + " FROM " + quotePath(e, req.Table)
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if len(order) > 0 {
		q += " ORDER BY " + strings.Join(order, ", ")
	}

	// 只要结构：不建会话、不取游标，写完结建语句就返回（`backup -type schema` 走这里）。
	// 放在这儿而不是写完之后再截断：截断要靠字符串匹配删 INSERT，删错一行就是数据错乱。
	if req.SchemaOnly {
		if format != ExportSQL {
			return nil, fmt.Errorf("%w: 只要结构时只能用 sql 格式", ErrExportFormat)
		}
		sbw := bufio.NewWriterSize(w, 64*1024)
		if err := e.writeSQLPreamble(ctx, sbw, req.Table, td); err != nil {
			return nil, err
		}
		if err := sbw.Flush(); err != nil {
			return nil, err
		}
		return &ExportResult{Columns: len(names), Duration: time.Since(started)}, nil
	}

	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()

	cur, err := sess.Query(ctx, q, args, driver.QueryOptions{FetchSize: pageSize})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close() }()

	out := &ExportResult{Columns: len(names)}
	bw := bufio.NewWriterSize(w, 64*1024)
	table := quotePath(e, req.Table)

	// SQL 脚本先写头与建表语句：拿到文件就能直接回放，不用另找结构。
	if format == ExportSQL {
		if err := e.writeSQLPreamble(ctx, bw, req.Table, td); err != nil {
			return nil, err
		}
	}

	switch format {
	case ExportCSV:
		cw := csv.NewWriter(bw)
		if req.Header {
			if err := cw.Write(names); err != nil {
				return nil, err
			}
		}
		for {
			batch, more, err := cur.Next(ctx)
			if err != nil {
				return nil, err
			}
			for _, row := range batch {
				rec := make([]string, len(row))
				for i, v := range row {
					rec[i] = csvCell(v)
				}
				if err := cw.Write(rec); err != nil {
					return nil, err
				}
				out.Rows++
			}
			if !more {
				break
			}
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			return nil, err
		}

	case ExportMarkdown:
		// 表头必写：req.Header 是 CSV 的开关，markdown 表格没有表头就是坏语法。
		bw.WriteString("| " + strings.Join(markdownCells(names), " | ") + " |\n")
		bw.WriteString("|" + strings.Repeat(" --- |", len(names)) + "\n")
		for {
			batch, more, err := cur.Next(ctx)
			if err != nil {
				return nil, err
			}
			for _, row := range batch {
				rec := make([]string, len(row))
				for i, v := range row {
					rec[i] = markdownCell(v)
				}
				bw.WriteString("| " + strings.Join(rec, " | ") + " |\n")
				out.Rows++
			}
			if !more {
				break
			}
		}

	case ExportSQL:
		for {
			batch, more, err := cur.Next(ctx)
			if err != nil {
				return nil, err
			}
			for _, row := range batch {
				vals := make([]string, len(insertIdx))
				insNames := make([]string, len(insertIdx))
				for k, i := range insertIdx {
					var v any
					if i < len(row) {
						v = row[i]
					}
					vals[k] = sqlLiteral(e.conn.Engine(), v)
					insNames[k] = e.dialect.QuoteIdent(td.Columns[i].Name)
				}
				fmt.Fprintf(bw, "INSERT INTO %s (%s) VALUES (%s);\n",
					table, strings.Join(insNames, ", "), strings.Join(vals, ", "))
				out.Rows++
			}
			if !more {
				break
			}
		}

	case ExportXLSX:
		xw, xerr := newXLSX(bw, req.Table)
		if xerr != nil {
			return nil, xerr
		}
		// 表头一律按文本写：列名不是数字，交给 Excel 猜只会得到右对齐的表头。
		if err := xw.Row(names, make([]bool, len(names))); err != nil {
			return nil, err
		}
		num := make([]bool, len(names))
		for {
			batch, more, err := cur.Next(ctx)
			if err != nil {
				return nil, err
			}
			for _, row := range batch {
				vals := make([]string, len(row))
				for i, v := range row {
					vals[i], num[i] = xlsxCell(v)
				}
				if err := xw.Row(vals, num); err != nil {
					return nil, err
				}
				out.Rows++
			}
			if !more {
				break
			}
		}
		if err := xw.Close(); err != nil {
			return nil, err
		}

	case ExportNDJSON:
		enc := json.NewEncoder(bw)
		for {
			batch, more, err := cur.Next(ctx)
			if err != nil {
				return nil, err
			}
			for _, row := range batch {
				m := make(map[string]any, len(row))
				for i, v := range row {
					if i < len(names) {
						m[names[i]] = jsonValue(v)
					}
				}
				if err := enc.Encode(m); err != nil {
					return nil, err
				}
				out.Rows++
			}
			if !more {
				break
			}
		}
	}

	if err := bw.Flush(); err != nil {
		return nil, err
	}

	out.Duration = time.Since(started)
	actor := actorOf("")
	e.record(actor, Request{SQL: q}, &Outcome{
		ExecutionID: newID(),
		Risk:        policy.R0,
		Friction:    policy.F0,
		Duration:    out.Duration,
	}, "ok", q, fmt.Sprintf("导出 %d 行（%s）", out.Rows, format))
	return out, nil
}

// markdownCell 把一个值写成 markdown 表格单元格。
//
// 与 csvCell 的区别不只是转义：这里把 NULL 与空串分开了。CSV 没法表达 NULL，
// 两者都只能是空，只能去读 ndjson 才知道区别；而 markdown 是给人对账用的，
// 一行「NULL」和一行「”」正是界面上显示的样子，不该在这个环节糊掉。
func markdownCell(v any) string {
	if v == nil {
		return "NULL"
	}
	if s, ok := v.(string); ok && s == "" {
		return "''"
	}
	return markdownEscape(csvCell(v))
}

// markdownCells 批量转义（表头也走这里，列名里的竖线同样会破表）。
func markdownCells(vals []string) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = markdownEscape(v)
	}
	return out
}

// markdownEscape 处理两处会破表的东西。
//
// 反斜杠必须先转：只把 | 换成 \| 的话，数据里本来就是 | 的会变成 \|，
// 渲染时前半截是转义后的反斜杠、后半截变成单元格分隔符——表格当场错位。
// 换行换成 <br>：markdown 表格里不能有真换行，写进去整行就断了。
func markdownEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r\n", "<br>")
	return strings.ReplaceAll(s, "\n", "<br>")
}

// csvCell 把一个值写成 CSV 单元格。
//
// NULL 写成空串 —— CSV 没有 NULL 表示法，这与「空字符串」在文本上无法区分。
// 需要区分的场合用 ndjson（那里 NULL 是真的 null）。
func csvCell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		if utf8.Valid(x) {
			return string(x)
		}
		return base64.StdEncoding.EncodeToString(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	default:
		return fmt.Sprint(v)
	}
}

// jsonValue 把一个值转成可 JSON 序列化的形态。
//
// BigInt 一律以字符串输出：JSON 数字在多数语言里按双精度解析，超过 2^53 会静默丢精度。
// 二进制以 base64 输出，非 UTF-8 文本强行当字符串会写出坏字节。
func jsonValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return base64.StdEncoding.EncodeToString(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	default:
		return v
	}
}

// sqlLiteral 把一个值写成 SQL 字面量，按引擎的写法。
//
// 差异只有两处，但两处都会让脚本在对方引擎上语法错误：二进制（X'..' 与
// '\x..'::bytea）和布尔（1/0 与 TRUE/FALSE）。其余类型用通用写法。
// 认不出来的类型按字符串引起来——宁可多一层隐式转换，也不要拼出半截语句。
func sqlLiteral(engine model.Engine, v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if engine == model.EnginePostgres {
			if x {
				return "TRUE"
			}
			return "FALSE"
		}
		if x {
			return "1"
		}
		return "0"
	case []byte:
		hex := hex.EncodeToString(x)
		if engine == model.EnginePostgres {
			return `'\x` + hex + `'::bytea`
		}
		return "X'" + hex + "'"
	case time.Time:
		return "'" + x.Format("2006-01-02 15:04:05.999999") + "'"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return fmt.Sprint(x)
	default:
		return "'" + strings.ReplaceAll(fmt.Sprint(x), "'", "''") + "'"
	}
}

// xlsxCell 把一个值转成 (单元格文本, 是否数字)。
//
// 数字单独标出来是因为 xlsx 有类型：都写成文本的话，金额与 ID 在 Excel 里会
// 左对齐、无法求和，用户第一件事就是选中整列改成数字。布尔写成 1/0：Excel 的
// TRUE/FALSE 是另一种单元格类型，跨软件往返时更容易变形。
//
// 时间写成 RFC3339 文本：Excel 的日期是「1900 起的天数」浮点，转换规则里还有
// 1900 闰年这个历史坑，宁可给人看 ISO 字符串，也不要在导出环节猜。
func xlsxCell(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case bool:
		if x {
			return "1", true
		}
		return "0", true
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return fmt.Sprint(x), true
	}
	return csvCell(v), false
}
