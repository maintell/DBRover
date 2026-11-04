package webui

import (
	"os"
	"strings"
	"testing"
)

// TestIrreversibleWarningIsRed 锁住 docs/07 验收第 3 条的视觉口径。
//
// 「不可逆」和「需要注意」不是一个量级：删列丢的是数据。这类回归没有编译
// 错误——把 danger 改回 warn 照样跑，所以只能靠断言钉住类名与配色。
func TestIrreversibleWarningIsRed(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	if !strings.Contains(string(js), "'<p class=\"danger\">该变更不可逆") {
		t.Error("不可逆提示必须用 danger 类（红色），不能和普通警示共用 warn")
	}

	css, err := os.ReadFile("style.css")
	if err != nil {
		t.Fatalf("读 style.css: %v", err)
	}
	if !strings.Contains(string(css), ".card p.danger { color: var(--danger); }") {
		t.Error("缺少 .card p.danger 的红色定义")
	}
	// 红色本身也要真的红：--danger 指到别处去了这个断言能发现。
	if !strings.Contains(string(css), "--danger: #f85149;") {
		t.Error("--danger 应当仍是红色 #f85149")
	}
}
