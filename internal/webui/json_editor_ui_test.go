package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUIJSONEditor 守住 JSON 列的专用编辑器（docs/06 §1.4）。
//
// 契约要求「JSON/JSONB：折叠摘要 + 专用编辑器（格式化 + 校验）」。一格 contenteditable
// 放不下也没法校验，所以走右键菜单里的专用入口。这里钉住：入口只在 JSON 列且可写时
// 出现、能格式化、校验失败不丢用户输入。
func TestUIJSONEditor(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{
		"'JSON 编辑器'",
		"(td.dataset.kind || '') === 'json'",
		"function openJsonEditor(td)",
		"function setCellText(td, text)",
		// 格式化与校验：能解析就美化，解析不了要说不合法。
		"JSON.stringify(JSON.parse",
		"JSON 不合法：",
		// 校验失败时不能把原文丢掉。
		"（内容还在，重新打开继续改）",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("JSON 编辑器缺少 %q", want)
		}
	}
}
