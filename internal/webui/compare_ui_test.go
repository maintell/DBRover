package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUICompareDistinguishesMatView 守住比对结果里「物化视图」这个标注。
//
// 物化视图与普通视图混在一张清单里，不标出来的话「升级会删掉它」会被读成
// 「只是一个视图」——而它还存着一份结果，重建要重算。
func TestUICompareDistinguishesMatView(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{
		"v.kind === 'matview'",
		"'视图 / 物化视图差异'",
		"（物化视图）",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("比对渲染里缺少 %q：物化视图会被当成普通视图", want)
		}
	}
}
