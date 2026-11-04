package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUICreateTypeIsWired 守住「新建枚举类型」这条入口在界面里真的接上了。
//
// 这类断言只能证明「代码写了」，不能证明「点了会跑」——所以行为本身另有真机
// 点击记录（docs/07 §8.1）。它挡的是另一类错：改了 id 或字段名，界面静静失灵，
// 而所有服务层用例照样绿。
func TestUICreateTypeIsWired(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	h, src := string(html), string(js)

	// 按钮默认 hidden：没有用户类型的引擎上不该闪一下再消失。
	if !strings.Contains(h, `id="btn-new-type"`) {
		t.Fatal("index.html 里没有 btn-new-type")
	}
	if !strings.Contains(h, `id="btn-new-type" title="新建枚举类型" hidden`) {
		t.Error("btn-new-type 应当默认 hidden，由能力位决定显示")
	}
	// 显示由能力位决定，而不是无条件摆出来再点出一个必然失败的错。
	if !strings.Contains(src, "$('btn-new-type').hidden = !caps.user_types") {
		t.Error("btn-new-type 的显示没有接在 capabilities.user_types 上")
	}
	// 点击绑定与实现都要在（绑定写在元素挂进文档之前会整页白掉，这条只挡漏写）。
	if !strings.Contains(src, "function showCreateType()") {
		t.Error("没有 showCreateType 实现")
	}
	if !strings.Contains(src, "$('btn-new-type').onclick = () => showCreateType();") {
		t.Error("btn-new-type 没有绑定点击")
	}
	// 请求体字段名必须与服务层一致：action=create_type + type{name,values}。
	// 写错时 HTTP 层会以「json: unknown field」400，界面只显示一句看不懂的错。
	if !strings.Contains(src, "action: 'create_type'") {
		t.Error("对话框没有发 create_type")
	}
	if !strings.Contains(src, "type: { name, values }") {
		t.Error("请求体字段与服务层的 type{name,values} 不一致")
	}
}
