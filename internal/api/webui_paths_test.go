package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWebuiUsesKnownPaths 校验界面里出现的每个接口路径都真实存在。
//
// 界面把路径集中在 app.js 的 API 常量里。端点改名而界面没跟上时，
// 这类 bug 只有点开页面才会暴露；这里把它变成编译期之外的机械检查。
func TestWebuiUsesKnownPaths(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "webui", "app.js"))
	if err != nil {
		t.Fatalf("读取 app.js: %v", err)
	}

	block := extractObjectBlock(string(src), "const API = {")
	if block == "" {
		t.Fatal("app.js 里找不到 const API 常量块")
	}

	paths := extractQuotedPaths(block)
	if len(paths) < 4 {
		t.Fatalf("只解析出 %d 条路径，解析逻辑可能已失效（应至少 4 条）: %v", len(paths), paths)
	}

	known := map[string]bool{}
	for _, rt := range New("t", nil, nil).routes() {
		known[rt.Path] = true
	}

	for _, p := range paths {
		if !known[p] {
			t.Errorf("界面引用了不存在的接口路径 %q", p)
		}
	}
}

// extractObjectBlock 取出以 header 开头、到首个 "};" 为止的片段。
func extractObjectBlock(src, header string) string {
	i := strings.Index(src, header)
	if i < 0 {
		return ""
	}
	rest := src[i:]
	if j := strings.Index(rest, "};"); j >= 0 {
		return rest[:j+2]
	}
	return ""
}

var quotedRe = regexp.MustCompile(`'([^']+)'`)
var paramRe = regexp.MustCompile(`:([a-z]+)`)

// extractQuotedPaths 取单引号字符串，并把 :id 规范成 OpenAPI 的 {id} 写法。
func extractQuotedPaths(block string) []string {
	var out []string
	for _, m := range quotedRe.FindAllStringSubmatch(block, -1) {
		v := m[1]
		if !strings.HasPrefix(v, "/") {
			continue
		}
		out = append(out, paramRe.ReplaceAllString(v, "{$1}"))
	}
	return out
}
