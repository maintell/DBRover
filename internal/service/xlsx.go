package service

import (
	"archive/zip"
	"bufio"
	"fmt"
	"io"
	"strings"
)

// xlsxWriter 边写边产出一个最小可用的 .xlsx。
//
// 为什么手写 OOXML 而不是引库：这一项只需要「一张表 + 表头 + 单元格」，
// 而 xlsx 是 zip + 几段 XML。为一个导出格式引一个第三方库（还常带 CGO 或庞大的
// 传递依赖）不值得——本项目对纯 Go 的要求是硬约束。
//
// 用的是 inlineStr 而不是 sharedStrings：省掉一张索引表，少一处可能写错的地方。
type xlsxWriter struct {
	zw   *zip.Writer
	bw   *bufio.Writer
	row  int
	cell int
}

// newXLSX 写入固定的四段元数据，并打开 sheet1 的写入流。
func newXLSX(w io.Writer, sheetName string) (*xlsxWriter, error) {
	zw := zip.NewWriter(w)
	meta := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
	<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
	<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
	<Default Extension="xml" ContentType="application/xml"/>
	<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
	<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
	</Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
	<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
	<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
	</Relationships>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
	<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
	<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
	</Relationships>`,
	}
	for name, body := range meta {
		fw, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(fw, body); err != nil {
			return nil, err
		}
	}
	// workbook 里带工作表名；Excel 对名字有限制（≤31 字符，不能含 []:*?/\\）。
	fw, err := zw.Create("xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(fw,
		`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="%s" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		sheetXMLName(sheetName)); err != nil {
		return nil, err
	}

	fs, err := zw.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(fs, 64*1024)
	bw.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	return &xlsxWriter{zw: zw, bw: bw}, nil
}

// Row 写一行。numeric[i] 为 true 时按数字写，否则按文本（inlineStr）。
func (x *xlsxWriter) Row(values []string, numeric []bool) error {
	x.row++
	if _, err := fmt.Fprintf(x.bw, `<row r="%d">`, x.row); err != nil {
		return err
	}
	for i, v := range values {
		ref := colLetter(i+1) + fmt.Sprint(x.row)
		if v == "" {
			continue // 空单元格直接跳过：xlsx 允许稀疏行，省体积也少一处转义
		}
		if i < len(numeric) && numeric[i] {
			if _, err := fmt.Fprintf(x.bw, `<c r="%s"><v>%s</v></c>`, ref, v); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(x.bw, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, xmlEscape(v)); err != nil {
			return err
		}
	}
	_, err := x.bw.WriteString(`</row>`)
	return err
}

// Close 收尾并关掉 zip。顺序不能反：XML 必须写完才关条目。
func (x *xlsxWriter) Close() error {
	if _, err := x.bw.WriteString(`</sheetData></worksheet>`); err != nil {
		return err
	}
	if err := x.bw.Flush(); err != nil {
		return err
	}
	return x.zw.Close()
}

// colLetter 把列序号（1 起）转成 A / Z / AA / AB 这种。
func colLetter(n int) string {
	if n <= 0 {
		return "A"
	}
	var b []byte
	for n > 0 {
		n--
		b = append([]byte{byte('A' + n%26)}, b...)
		n /= 26
	}
	return string(b)
}

// sheetXMLName 把表名收拾成 Excel 能接受的工作表名。
//
// Excel 的限制：≤31 字符，不能出现 [ ] : * ? / \\。违反时文件能写出来但打不开，
// 而报错是「文件已损坏」——那种错误信息完全指不到真正的起因。
func sheetXMLName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Sheet1"
	}
	repl := func(r rune) rune {
		if strings.ContainsRune("[]:*?/\\", r) {
			return '_'
		}
		return r
	}
	name = strings.Map(repl, name)
	runes := []rune(name)
	if len(runes) > 31 {
		name = string(runes[:31])
	}
	return name
}

// xmlEscape 处理 XML 里必须转义的字符。
//
// 与 HTML 的区别：XML 不认 &nbsp; 之类的命名实体，也不该把换行变成 <br>——
// 单元格里可以有多行文本，用 xml:space="preserve" 保真就够了。
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
