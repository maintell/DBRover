package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUIActionsExistInService 让「界面发了个服务端不认识的 action」变红。
//
// 这类错在运行时只表现为一句含糊的业务错误（或干脆没反应），而界面代码里就是
// 一行字符串。判据取自动作常量的定义处，不从界面自己声明——两边各写一份清单，
// 写错也不会红。
func TestUIActionsExistInService(t *testing.T) {
	svc, err := os.ReadFile("../service/ddl.go")
	if err != nil {
		t.Fatal(err)
	}
	ui, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}

	known := map[string]bool{}
	for _, a := range extractBetween(string(svc), "DDLAction = ", "\n") {
		known[strings.Trim(a, "\" ")] = true
	}
	if len(known) < 5 {
		t.Fatalf("只从 ddl.go 里认出 %d 个动作，解析恐怕失效了", len(known))
	}

	// 界面把动作写成 action: 'xxx'。
	used := extractBetween(string(ui), "action: '", "'")
	if len(used) == 0 {
		t.Fatal("没在 app.js 里找到任何 action 字面量，解析恐怕失效了")
	}
	for _, a := range used {
		if !known[a] {
			t.Errorf("界面发起的 action %q 在 service/ddl.go 里没有对应常量——点了只会得到一句含糊的错误", a)
		}
	}
}

// extractBetween 取出 prefix 与 suffix 之间的片段（不含两端），循环直到结尾。
//
// 用手写扫描而不是正则：这两处的模式都很简单，正则反而要多写一层转义，
// 而转义写错的测试本身没人测。
func extractBetween(s, prefix, suffix string) []string {
	var out []string
	for {
		i := strings.Index(s, prefix)
		if i < 0 {
			return out
		}
		s = s[i+len(prefix):]
		j := strings.Index(s, suffix)
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+len(suffix):]
	}
}
