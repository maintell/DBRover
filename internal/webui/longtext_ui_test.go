package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUILongText 守住长文本的「截断显示 + 全文编辑器」（docs/06 §1.4）。
//
// 这条最要紧的不是好看，而是**不要让截断变成数据截断**：格子里显示摘要、失焦时
// 提交格子的 textContent——如果长文本格仍可编辑，一次误点就会把正文截成 200 字符
// 写回库里。所以长文本格必须不可编辑，改走右键的全文编辑器。
func TestUILongText(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{
		"const longTextLimit = 200",
		// 长文本格不进 contentEditable 分支，而是提前 return。
		"if (!isNull && full.length > longTextLimit) {",
		"paintLongText(td, full);",
		"td.classList.add('longtext');",
		"return;",
		// 全文编辑器入口与实现。
		"'长文本编辑器'",
		"function openTextEditor(td, writable)",
		// 只读连接只能看。
		"$('long-text').readOnly = true",
		// 写回后按长度重画，别把整段正文塞回一格。
		"if (text.length > longTextLimit) {",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("长文本处理缺少 %q", want)
		}
	}
}
