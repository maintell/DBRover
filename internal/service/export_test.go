package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/service"
)

func TestExportCSV(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	// 造一个带逗号与引号的值，验证 CSV 转义；再来一行 NULL 与空串。
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO t (id, v) VALUES (2, 'a,b')`,
		`INSERT INTO t (id, v) VALUES (3, 'say "hi"')`,
		`INSERT INTO t (id, v) VALUES (4, NULL)`,
		`INSERT INTO t (id, v) VALUES (5, '')`,
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("准备数据 (%s): %v", q, err)
		}
	}
	_ = s.Close()

	var buf bytes.Buffer
	res, err := ex.Export(ctx, service.ExportRequest{
		Table: "t", Format: service.ExportCSV, Header: true,
	}, &buf)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.Rows != 5 {
		t.Errorf("导出行数 = %d, 期望 5", res.Rows)
	}
	if res.Columns != 2 {
		t.Errorf("导出列数 = %d, 期望 2", res.Columns)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("应有 1 行表头 + 5 行数据, 实际 %d 行: %v", len(lines), lines)
	}
	if lines[0] != "id,v" {
		t.Errorf("表头 = %q, 期望 id,v", lines[0])
	}
	// 逗号与引号必须被转义，否则表格软件会把它切成多列。
	if !strings.Contains(buf.String(), `"a,b"`) {
		t.Errorf("含逗号的值没有加引号: %v", lines)
	}
	if !strings.Contains(buf.String(), `"say ""hi"""`) {
		t.Errorf("引号没有按 CSV 规则翻倍: %v", lines)
	}
	// NULL 与空串在 CSV 里都是空单元格（文本上无法区分，已写入文档）。
	if !strings.Contains(buf.String(), "4,") {
		t.Errorf("NULL 行没写成空单元格: %v", lines)
	}
}

func TestExportNDJSONPreservesPrecisionAndNull(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	big := int64(9007199254740993) // 2^53+1：双精度会丢成 …992
	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, `INSERT INTO t (id, v) VALUES (?, NULL)`, []any{big}); err != nil {
		t.Fatalf("插大整数: %v", err)
	}
	_ = s.Close()

	var buf bytes.Buffer
	if _, err := ex.Export(ctx, service.ExportRequest{
		Table: "t", Format: service.ExportNDJSON,
	}, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 行（setup 的 1 行 + 插入的 1 行）, 实际 %d: %v", len(lines), lines)
	}
	var bigRow map[string]any
	for _, ln := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("不是合法 JSON 行: %v (%s)", err, ln)
		}
		// 大整数必须以字符串出现，否则消费端按双精度解析会静默丢精度。
		if _, ok := rec["id"].(string); !ok {
			t.Errorf("id 应以字符串输出，实际 %T (%v)", rec["id"], rec["id"])
		}
		if rec["id"] == "9007199254740993" {
			bigRow = rec
		}
	}
	if bigRow == nil {
		t.Fatalf("没找到大整数那行: %s", buf.String())
	}
	if bigRow["v"] != nil {
		t.Errorf("v 应输出 null，实际 %v", bigRow["v"])
	}
	if !strings.Contains(buf.String(), "9007199254740993") {
		t.Errorf("大整数丢了精度: %s", buf.String())
	}
}

func TestExportFilterAndOrder(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)
	seed(t, e, 2, 9) // id 1..10，v 全是 'x'

	var buf bytes.Buffer
	res, err := ex.Export(ctx, service.ExportRequest{
		Table: "t", Format: service.ExportCSV, Header: true,
		Filter: []service.FilterCond{{Column: "id", Op: service.FilterGe, Value: "9"}},
		Order:  &service.OrderSpec{Column: "id", Desc: true},
	}, &buf)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.Rows != 2 {
		t.Fatalf("id>=9 应为 2 行, 实际 %d", res.Rows)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if lines[1] != "10,x" || lines[2] != "9,x" {
		t.Errorf("排序未生效: %v", lines)
	}
}

func TestExportRejectsBadFormatAndColumn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	var buf bytes.Buffer
	if _, err := ex.Export(ctx, service.ExportRequest{
		Table: "t", Format: service.ExportFormat("xml"),
	}, &buf); !errors.Is(err, service.ErrExportFormat) {
		t.Errorf("应返回 ErrExportFormat, 实际 %v", err)
	}

	if _, err := ex.Export(ctx, service.ExportRequest{
		Table: "t", Format: service.ExportCSV,
		Filter: []service.FilterCond{{Column: "nope", Op: service.FilterEq, Value: "1"}},
	}, &buf); !errors.Is(err, service.ErrInvalidFilter) {
		t.Errorf("应返回 ErrInvalidFilter, 实际 %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("失败时不该写出任何数据, 实际 %d 字节", buf.Len())
	}
}

// TestExportWorksWithoutPrimaryKey 是设计取舍的回归：
// 浏览需要主键（键集分页），导出不需要——视图与无主键表也必须能导。
func TestExportWorksWithoutPrimaryKey(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, q := range []string{
		`CREATE TABLE nopk3 (a TEXT, b TEXT)`,
		`INSERT INTO nopk3 (a, b) VALUES ('1', 'x'), ('2', 'y')`,
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("准备数据 (%s): %v", q, err)
		}
	}
	_ = s.Close()

	var buf bytes.Buffer
	res, err := ex.Export(ctx, service.ExportRequest{
		Table: "nopk3", Format: service.ExportCSV, Header: true,
	}, &buf)
	if err != nil {
		t.Fatalf("无主键表导出失败: %v", err)
	}
	if res.Rows != 2 {
		t.Errorf("导出行数 = %d, 期望 2", res.Rows)
	}
}
