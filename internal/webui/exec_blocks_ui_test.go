package webui

import (
	"os"
	"strings"
	"testing"
)

// TestRenderExecPerStatementBlock 守住多语句结果的渲染方式（docs/05 §3.2）。
//
// 这里连着踩过两次同一个坑，两次都是「不报错、只是结果少了一半」：
//  1. 把表格直接画进结果区（append=false 会 replaceChildren）→ 只剩最后一条语句；
//  2. 标题加进块之后又把块直接交给 renderTable → 标题被它 replaceChildren 清掉。
//
// 所以断言不是「有标题」这种外观描述，而是**隔离**：每条语句一个容器、表格画在
// 独立的一层里、标题只在多条语句时出现。前两条任何一种退回原样，界面上都会
// 静默丢结果。
func TestRenderExecPerStatementBlock(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)

	for _, want := range []string{
		// 每条语句一个块。
		"const box = document.createElement('div');",
		"box.className = 'result-block';",
		// 表格必须画在块内的独立一层：直接传 box 会被 renderTable 清空。
		"const view = document.createElement('div');",
		"renderTable(view, r.columns, r.rows || [], false, false);",
		"box.appendChild(view);",
		// 标题只在多条语句时出现。
		"const multi = list.length > 1;",
		"if (multi) box.appendChild(resultHead(r, i));",
		// 标题内容与跳转。
		"'第 ' + (i + 1) + ' 条 · ' + headOfSQL(r.sql)",
		"el.setSelectionRange(at, at + needle.length);",
		// 失败块也走同一个容器：否则「第 N 条」与失败信息会各占一处、对不上。
		"d.textContent = '失败：' + r.err",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("多语句结果渲染缺少 %q", want)
		}
	}

	// 反向断言：不允许再把结果区直接交给 renderTable（那正是第一次的 bug）。
	if strings.Contains(src, "renderTable($('results'), r.columns") {
		t.Error("表格不该直接画进结果区：renderTable 会 replaceChildren，多语句只剩最后一条")
	}
}
