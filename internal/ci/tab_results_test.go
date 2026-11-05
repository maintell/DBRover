package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTabResultContractMatchesDocs 守住 ADR-0017 多标签编辑器「结果区」这一处
// 文档与实现的一致性。批准的设计曾写过「每个标签独立的语句与结果」，而实现是
// 「每个标签独立的语句 + 全局结果区只显示最近一次执行，切标签清空并提示重跑」：
// 结果网格的行内编辑与审批按钮带事件绑定，渲染读的是 app.js 里共享的全局 state
// （tableName / pending / editable / gridColumns / undo）。按标签快照结果若用
// innerHTML 回放会变成死按钮；若与共享 state 混用，某个标签的编辑按钮会读到
// 另一个标签的表名而误改数据。本轮追加式改动不重做结果区状态模型，因此文档把
// 它写成已知限制。这条门禁把「设计/ADR 的降级口径」与「ui2.js 的清空行为」钉在
// 一起：任一侧单方面改回过度承诺、或偷偷去掉清空，都会红。
func TestTabResultContractMatchesDocs(t *testing.T) {
	root := repoRoot(t)
	read := func(rel ...string) string {
		b, err := os.ReadFile(filepath.Join(append([]string{root}, rel...)...))
		if err != nil {
			t.Fatalf("读 %v: %v", rel, err)
		}
		return string(b)
	}

	ui2 := read("internal", "webui", "ui2.js")
	spec := read("docs", "superpowers", "specs", "2026-11-06-desktop-shell-and-ux-design.md")
	adr := read("docs", "adr", "0017-desktop-shell-and-settings.md")

	// 扫描覆盖自检：路径写错会读到空串，整条门禁会假绿。
	for name, doc := range map[string]string{"ui2.js": ui2, "spec": spec, "ADR-0017": adr} {
		if len(doc) < 200 {
			t.Errorf("%s 只读到 %d 字节，扫描恐怕失效", name, len(doc))
		}
	}

	// 实现侧：切标签/新建/关闭都走 resultHint 清空结果区，而不是按标签快照结果 DOM。
	for _, fn := range []string{"function switchTab(", "function addTab(", "function closeTab(", "function resultHint("} {
		if !strings.Contains(ui2, fn) {
			t.Errorf("ui2.js 缺少 %q", fn)
		}
	}
	if !strings.Contains(ui2, "r.replaceChildren()") {
		t.Error("ui2.js 的 resultHint 没有清空结果区")
	}
	// 覆盖性：定义 1 处 + 三个切标签入口各 1 处，共 4 处 resultHint( 调用；
	// 漏掉任一入口就会残留上一个标签的结果。
	if n := strings.Count(ui2, "resultHint("); n < 4 {
		t.Errorf("ui2.js 只有 %d 处 resultHint(，切标签清空结果的行为不完整（应为定义 + switchTab/addTab/closeTab 三处调用，共 4）", n)
	}

	// 文档侧：设计与 ADR 都写成「全局结果区 / 已知限制」，不得再承诺每个标签独立结果。
	for name, doc := range map[string]string{"spec": spec, "ADR-0017": adr} {
		if !strings.Contains(doc, "结果区是全局的") {
			t.Errorf("%s 没有写明「结果区是全局的」降级口径", name)
		}
		if !strings.Contains(doc, "已知限制") {
			t.Errorf("%s 没有把结果区不按标签隔离标为已知限制", name)
		}
		const overclaim = "每个标签独立的语句与结果"
		if strings.Contains(doc, overclaim) {
			t.Errorf("%s 又写回了「%s」——实现并未按标签隔离结果（要么改实现，要么先改设计）", name, overclaim)
		}
	}
}
