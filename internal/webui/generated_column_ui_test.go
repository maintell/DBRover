package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUIGeneratedColumnIsReadOnly 守住「生成列不能直接改」。
//
// 后端从 PRAGMA table_xinfo 读到生成列之后，列清单里就多了它。界面若照常把它做成
// 可编辑单元格，用户改一下、失焦、提交——引擎直接拒绝，而错误出现在一次「看起来
// 只是改了个格子」的操作之后。这条门禁钉住：attachEdit 见到 is_generated 直接返回。
func TestUIGeneratedColumnIsReadOnly(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{
		"if (column && column.is_generated) {",
		"td.classList.add('generated')",
		"生成列：由引擎按表达式计算，不能直接改",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("生成列只读守卫缺少 %q", want)
		}
	}
}
