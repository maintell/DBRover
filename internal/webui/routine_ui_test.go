package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUICreateRoutineIsWired 守住「新建例程」这条入口在界面里真的接上了。
//
// 同 TestUICreateTypeIsWired：只证明接线在，行为另由真机用例与浏览器实测覆盖。
func TestUICreateRoutineIsWired(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	h, src := string(html), string(js)

	if !strings.Contains(h, `id="btn-new-routine" title="新建存储过程 / 函数" hidden`) {
		t.Fatal("btn-new-routine 应当在 index.html 里、且默认 hidden")
	}
	if !strings.Contains(src, "$('btn-new-routine').hidden = !caps.add_routine") {
		t.Error("btn-new-routine 的显示没有接在 capabilities.add_routine 上")
	}
	if !strings.Contains(src, "function showCreateRoutine()") {
		t.Error("没有 showCreateRoutine 实现")
	}
	if !strings.Contains(src, "$('btn-new-routine').onclick = () => showCreateRoutine();") {
		t.Error("btn-new-routine 没有绑定点击")
	}
	// 新建是默认，替换必须显式勾选：`replace ? 'replace_routine' : 'create_routine'`。
	if !strings.Contains(src, "action: replace ? 'replace_routine' : 'create_routine'") {
		t.Error("对话框没有区分新建与整段替换")
	}
	if !strings.Contains(src, `id="rt-replace"`) {
		t.Error("没有「整段替换」勾选框")
	}
	// 请求体字段必须与服务层的 routine{name,kind,params,returns,body} 对齐。
	for _, field := range []string{"name:", "kind:", "params,", "returns:", "body:"} {
		if !strings.Contains(src, field) {
			t.Errorf("请求体缺少字段 %s", field)
		}
	}
}
