package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUISearchIsWired 守住对象搜索（FR-META-04）在界面里的接线。
//
// 这条需求此前是「驱动实现了、端点没有、界面更没有」，所以既要有接口用例
// （internal/api 的 TestSearchEndpoint），也要有界面接线：输入框存在、调对了
// 端点、与对象树共用同一份渲染（否则搜索结果点不开）。
func TestUISearchIsWired(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	h, src := string(html), string(js)

	if !strings.Contains(h, `id="obj-search"`) {
		t.Fatal("index.html 里没有搜索框 obj-search")
	}
	if !strings.Contains(src, "search: '/api/v1/connections/:id/search'") {
		t.Error("没有声明搜索端点")
	}
	if !strings.Contains(src, "function searchObjects()") {
		t.Error("没有 searchObjects 实现")
	}
	if !strings.Contains(src, "$('obj-search').oninput") {
		t.Error("搜索框没有绑定输入事件")
	}
	// 搜索结果必须走与对象树同一个渲染函数：两套渲染长歪之后，「搜索结果点不开」
	// 这种差异只会出现在搜索那条路上。
	if !strings.Contains(src, "function renderObjects(items)") ||
		!strings.Contains(src, "renderObjects(items);") {
		t.Error("搜索结果与对象树没有共用 renderObjects")
	}
	// key-value 连接没有对象清单，搜索框要藏起来，而不是点了报 400。
	if !strings.Contains(src, "$('obj-search').hidden = true") {
		t.Error("key-value 连接上应当隐藏搜索框")
	}
}
