package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTabResultContractMatchesDocs 守住 ADR-0017 多标签编辑器「每个标签独立的语句
// 与结果」这一处文档与实现的一致性。
//
// 实现机制（ui2.js，追加层、app.js 零改动）：包装全局 renderExec，每次执行后把
// #results 的活 DOM 节点引用存进当前标签；switchTab / addTab / closeTab 用
// restoreTabResult 把对应标签的节点搬回 #results。SQL 执行结果由 renderExec 渲染成
// 只读块（allowEdit=false、不设 dataset.key、不读 state.tableName/pending），搬活节点
// 不会丢 onclick、也不会把可编辑网格的共享 state 带进某个标签。可编辑的表浏览网格走
// loadTablePage、不经 renderExec，因此不参与标签隔离。
//
// 这条门禁把「设计/ADR 的承诺」与「ui2.js 的接关键」钉在一起：任一侧单方面退回
// 「全局结果区/已知限制」、或偷偷删掉 renderExec 包装/restoreTabResult，都会红。
// 端到端行为（两个标签各跑一条 SELECT、来回切、各自结果恢复）另由内置浏览器点验。
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
	appjs := read("internal", "webui", "app.js")
	spec := read("docs", "superpowers", "specs", "2026-11-06-desktop-shell-and-ux-design.md")
	adr := read("docs", "adr", "0017-desktop-shell-and-settings.md")

	// 扫描覆盖自检：路径写错会读到空串，整条门禁会假绿。
	for name, doc := range map[string]string{"ui2.js": ui2, "app.js": appjs, "spec": spec, "ADR-0017": adr} {
		if len(doc) < 200 {
			t.Errorf("%s 只读到 %d 字节，扫描恐怕失效", name, len(doc))
		}
	}

	// 实现侧接关键：包装 renderExec 捕获活节点 + 按标签恢复。
	for _, want := range []string{
		"window.renderExec = function (out)",
		"origRenderExec(out)",
		"t.result = Array.prototype.slice.call(r.childNodes)",
		"function restoreTabResult(",
		"r.appendChild(t.result[k])",
	} {
		if !strings.Contains(ui2, want) {
			t.Errorf("ui2.js 缺少 per-tab 结果接线 %q", want)
		}
	}
	// 三个切标签入口都必须改为 restoreTabResult，不能留 resultHint 清空；否则切回去
	// 只看到「执行以查看结果」，等于结果没按标签保存。
	for _, fn := range []string{"function switchTab(", "function addTab(", "function closeTab(", "function restoreTabResult("} {
		if !strings.Contains(ui2, fn) {
			t.Errorf("ui2.js 缺少 %q", fn)
		}
	}
	if n := strings.Count(ui2, "restoreTabResult("); n < 4 {
		t.Errorf("ui2.js 只有 %d 处 restoreTabResult(（应为定义 + switchTab/addTab/closeTab 三处调用，共 4）", n)
	}
	// 新标签要带 result:null 字段，否则未执行的标签没有结果槽位。
	if !strings.Contains(ui2, "result: null") {
		t.Error("ui2.js 的标签对象没有 result 字段")
	}
	// 结果块必须是只读渲染：renderExec 里 renderTable 的 allowEdit 必须为 false。
	// 一旦变成 true，标签里就会出现可编辑网格，读取共享 state 会串表误改数据。
	if !strings.Contains(appjs, "renderTable(view, r.columns, r.rows || [], false, false)") {
		t.Error("app.js 的 renderExec 不再是只读结果块，per-tab 活节点搬运会带上可编辑网格")
	}

	// 文档侧：设计与 ADR 都承诺「每个标签独立的语句与结果」，不得退回「已知限制」口径。
	for name, doc := range map[string]string{"spec": spec, "ADR-0017": adr} {
		if !strings.Contains(doc, "每个标签独立的语句与结果") {
			t.Errorf("%s 没有写明「每个标签独立的语句与结果」契约", name)
		}
		if !strings.Contains(doc, "活节点") {
			t.Errorf("%s 没有写明按活节点保存/恢复结果的实现机制", name)
		}
		const stale = "结果区是全局的"
		if strings.Contains(doc, stale) {
			t.Errorf("%s 还残留「%s」的旧降级口径——实现已改为 per-tab 结果", name, stale)
		}
	}
}
