package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUIPartitionsSection 守住结构页的分区区（FR-META-03）。
//
// 后端把 partitions 放进表详情之后，界面不画出来等于没做——这仓库已经踩过
// 「能力有、入口没有」好几次（对象搜索、行级增删、会话）。
func TestUIPartitionsSection(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{
		"const parts = out.partitions || []",
		"'分区（' + parts.length + '）'",
		// 四个字段缺一个，运维就看不出「范围多大、边界在哪」。
		"'名称', '方法', '表达式', '边界'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("结构页的分区区缺少 %q", want)
		}
	}
}
