package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUICapabilityKeysExist 让「界面读了一个不存在的能力位」变红。

// 界面按能力位决定摆不摆某个入口。键名写错时不会有任何报错：表达式取到
// undefined，那块 UI 就静静消失——用户以为「这个引擎不支持」，其实是拼错了。
// 判据取自动作定义处（model.Capabilities 的 json 标签），不从界面自证。
func TestUICapabilityKeysExist(t *testing.T) {
	ui, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	used := capabilityRefs(string(ui))
	if len(used) == 0 {
		t.Fatal("没在 app.js 里找到 capabilities.xxx 引用，扫描恐怕失效了")
	}
	known := capabilityKeys(t)
	if len(known) < 10 {
		t.Fatalf("只从 model.Capabilities 里认出 %d 个键，解析恐怕失效了", len(known))
	}
	for _, k := range used {
		if !known[k] {
			t.Errorf("界面读了 capabilities.%s，但 model.Capabilities 里没有这个 json 键——那块 UI 会静默消失", k)
		}
	}
}

// capabilityRefs 取出 app.js 里所有 capabilities.<ident> 的 ident（去重保序）。
func capabilityRefs(src string) []string {
	const prefix = "capabilities."
	seen := map[string]bool{}
	var out []string
	for i := 0; ; {
		j := strings.Index(src[i:], prefix)
		if j < 0 {
			return out
		}
		i += j + len(prefix)
		start := i
		for i < len(src) && isIdentChar(src[i]) {
			i++
		}
		if id := src[start:i]; id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
}

func isIdentChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// capabilityKeys 从 model.go 的 Capabilities 结构里取 json 键。
func capabilityKeys(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("../model/model.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "type Capabilities struct {")
	if i < 0 {
		t.Fatal("没找到 Capabilities 结构定义")
	}
	s = s[i:]
	end := strings.Index(s, "\n}")
	if end < 0 {
		t.Fatal("Capabilities 结构没有结束）")
	}
	out := map[string]bool{}
	for _, tag := range extractBetween(s[:end], "json:\"", "\"") {
		out[strings.Split(tag, ",")[0]] = true
	}
	return out
}
