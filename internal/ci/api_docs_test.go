package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	routeDefRe  = regexp.MustCompile(`"(/api/v1/[^"]+)"`)
	pathParamRe = regexp.MustCompile(`\{[A-Za-z_]+\}`)
	// 契约表里「标了 ✅ 的一行」：`METHOD` | `路径` | 说明 | ✅ …
	okRowRe = regexp.MustCompile("(?m)^\\|\\s*\x60[A-Z]+\x60\\s*\\|\\s*\x60(/[^\x60]+)\x60\\s*\\|[^\\n]*✅")
)

// normRoute 把路径参数名归一：{id} 与 {connection_id} 指的是同一类占位。
func normRoute(p string) string {
	return pathParamRe.ReplaceAllString(p, "{x}")
}

// TestDocImplementedEndpointsExist 守住「契约里标了 ✅ 的端点必须真的存在」。
//
// 为什么只查 ✅ 的行：docs/12 主体是**设计契约**，里面大量端点还没实现
// （`/connections/{id}/open`、`/monitor/*`、`/changes/{id}/approve` 这些，有的
// 被后来的方案取代，有的还排在待办里）。要求它们全都存在是错的：那会把设计意图
// 当成缺陷。**标了 ✅ 才是「已经实现」的承诺**，那种路径改一次名而文档没跟上，
// 客户端照着写就会 404。
//
// ADR 不在此列——那是设计记录，可能已被实现改写（ADR-0007 的 KV 路径就是，
// 已在原文补了实现口径）。
func TestDocImplementedEndpointsExist(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "internal", "api", "server.go"))
	if err != nil {
		t.Fatalf("读路由表: %v", err)
	}
	var routes []string
	for _, m := range routeDefRe.FindAllStringSubmatch(string(src), -1) {
		routes = append(routes, normRoute(m[1]))
	}
	if len(routes) < 20 {
		t.Fatalf("只从路由表里认出 %d 条，解析恐怕失效了", len(routes))
	}

	b, err := os.ReadFile(filepath.Join(root, "docs", "12-api-contract.md"))
	if err != nil {
		t.Fatalf("读 docs/12: %v", err)
	}
	rows := okRowRe.FindAllStringSubmatch(string(b), -1)
	if len(rows) < 5 {
		t.Fatalf("只扫到 %d 行标了 ✅ 的端点，扫描恐怕失效了", len(rows))
	}
	var missing []string
	for _, r := range rows {
		cite := normRoute(strings.TrimSpace(r[1]))
		if strings.Contains(cite, "?") {
			cite = cite[:strings.Index(cite, "?")]
		}
		found := false
		for _, route := range routes {
			if route == cite || strings.HasSuffix(route, cite) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, cite)
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("docs/12 把 %s 标成了 ✅，但服务端没有这条路由（改名/删除后要同步契约）", m)
	}
}
