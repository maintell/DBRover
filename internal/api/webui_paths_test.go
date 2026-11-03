package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
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

// TestWebuiEngineNamesAreAccepted 锁住「界面能选的引擎，服务端必须认识」。
//
// 这条是补一个真实缺陷：界面里 PostgreSQL 的选项值曾是 `pg`，而服务端
// （model.EnginePostgres）只认 `postgres`，选中它就得到「暂不支持的引擎」。
// 验收脚本一直显式传 `-Engine postgres`，所以没暴露出来。
func TestWebuiEngineNamesAreAccepted(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "webui", "app.js"))
	if err != nil {
		t.Fatalf("读取 app.js: %v", err)
	}
	block := extractObjectBlock(string(src), "const ENGINES = [")
	if block == "" {
		t.Fatal("app.js 里找不到 ENGINES 常量块")
	}

	re := regexp.MustCompile(`\['([a-z0-9_]+)'\s*,`)
	matches := re.FindAllStringSubmatch(block, -1)
	if len(matches) < 3 {
		t.Fatalf("只解析出 %d 个引擎，解析逻辑可能已失效: %s", len(matches), block)
	}

	// 用 model.AllEngines 而不是手写清单：手写的清单会和枚举脱节，
	// 而这正是本条门禁要防的那类不一致。
	known := map[model.Engine]bool{}
	for _, e := range model.AllEngines() {
		known[e] = true
	}
	for _, m := range matches {
		name := model.Engine(m[1])
		if !known[name] {
			t.Errorf("界面提供了服务端不认识的引擎名 %q", m[1])
		}
	}
}
