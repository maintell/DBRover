package webui

import (
	"os"
	"strings"
	"testing"
)

// TestColumnActionsOfferNullableAndDefault 守住列级操作在界面上真的有入口。
//
// 后端支持 `set_nullable` / `set_default` 而界面没有按钮，就等于这两个能力
// 只存在于 curl 里——这次的导出格式就是这么缺了 SQL 那一项的。
func TestColumnActionsOfferNullableAndDefault(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, want := range []struct{ name, needle string }{
		{"改可空入口", "'set_nullable'"},
		{"改默认值入口", "'set_default'"},
		{"默认值只收字面量的说明", "只接受字面量"},
		{"三件套复用", "async function planColumnChange("},
		{"加检查约束入口", "async function planAddCheck("},
		{"按能力位决定摆不摆", "capabilities.add_check"},
		{"新建视图入口", "async function showCreateView("},
		{"新建视图的按钮绑定", "$('btn-new-view').onclick"},
		{"新建序列入口", "async function showCreateSequence("},
		{"新建序列按能力位隐藏", "btn-new-seq"},
	} {
		if !strings.Contains(src, want.needle) {
			t.Errorf("app.js 里缺少「%s」：%q", want.name, want.needle)
		}
	}
}
