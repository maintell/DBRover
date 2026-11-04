package service_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/service"
)

// TestExportXLSX 验证 xlsx 是个真能打开的包：zip 结构齐、单元格类型对、转义正确。
//
// 这条只到「结构正确」这一层；能不能被 Excel 打开由另一条真机验证（CHANGELOG 里记录）。
func TestExportXLSX(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(ctx, `DELETE FROM t`, nil); err != nil {
		t.Fatalf("清空: %v", err)
	}
	// 刻意放三类容易出错的值：需要 XML 转义的文本、数字、NULL。
	if _, err := s.Execute(ctx, `INSERT INTO t (id, v) VALUES (1, ?)`, []any{"a<b&c\"d"}); err != nil {
		t.Fatalf("插文本: %v", err)
	}
	if _, err := s.Execute(ctx, `INSERT INTO t (id, v) VALUES (2, NULL)`, nil); err != nil {
		t.Fatalf("插 NULL: %v", err)
	}
	_ = s.Close()

	var buf bytes.Buffer
	res, err := ex.Export(ctx, service.ExportRequest{Table: "t", Format: service.ExportXLSX}, &buf)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.Rows != 2 {
		t.Fatalf("导出 %d 行，期望 2", res.Rows)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("不是合法 zip：%v", err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, oerr := f.Open()
		if oerr != nil {
			t.Fatal(oerr)
		}
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		files[f.Name] = string(body)
	}
	for _, want := range []string{
		"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml",
		"xl/_rels/workbook.xml.rels", "xl/worksheets/sheet1.xml",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("缺 %s：Excel 会报文件已损坏", want)
		}
	}

	sheet := files["xl/worksheets/sheet1.xml"]
	// 表头按文本写。
	if !strings.Contains(sheet, `<c r="A1" t="inlineStr"><is><t xml:space="preserve">id</t></is></c>`) {
		t.Errorf("表头单元格不对：%s", sheet)
	}
	// 数字按数字写（没有 t 属性）。
	if !strings.Contains(sheet, `<c r="A2"><v>1</v></c>`) {
		t.Errorf("数字单元格应无 t 属性：%s", sheet)
	}
	// 文本必须转义。
	if !strings.Contains(sheet, "a&lt;b&amp;c&quot;d") {
		t.Errorf("XML 转义没做：%s", sheet)
	}
}
