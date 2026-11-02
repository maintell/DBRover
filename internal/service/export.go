package service

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/csv"
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
)

// Valid 判断格式是否受支持。
func (f ExportFormat) Valid() bool { return f == ExportCSV || f == ExportNDJSON }

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
	for i, c := range td.Columns {
		cols[i] = e.dialect.QuoteIdent(c.Name)
		names[i] = c.Name
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
