package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUIRowActions 守住行级操作（FR-DATA-02 的删除与复制）。
//
// 服务层早就支持 insert / delete，界面却只有单元格编辑——P0 需求等于做了一半。
// 这里钉住四件事：菜单入口存在、删除带整行 before（只给主键会让回滚插回空壳）、
// 复制不复制主键、只读连接不摆这些入口。
func TestUIRowActions(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)

	for _, want := range []string{
		"label: '删除本行'",
		"label: '复制本行'",
		"function deleteRow(",
		"function copyRow(",
		"function submitRowChange(",
		// 删除必须带整行 before：服务端用它生成回滚，只给主键会插回缺列的空壳。
		"before: rowValues(tr)",
		// 复制不复制主键：自增主键复制过来必然撞键。
		"pks.includes(name.toLowerCase())",
		// 只读连接不摆行级入口。
		"state.current.access_mode !== 'read-only'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("行级操作缺少 %q", want)
		}
	}
}
