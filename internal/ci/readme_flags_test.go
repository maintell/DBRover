package ci

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestReadmeCoversEveryFlag 守住 AGENTS.md 的「README 参数表必须覆盖代码里每个开关」。
//
// 这条门禁是真被打破过才加的：serve 加了 -metrics、-audit 之后 README 只写了
// -open 与 -audit-anchor，读者按 README 操作根本不知道留存要加开关。手写的清单
// 会跟着代码一起漏，所以从 main.go 的 AST 里取开关名，README 里必须逐个出现。
func TestReadmeCoversEveryFlag(t *testing.T) {
	flags := flagsInMain(t)
	if len(flags) < 15 {
		t.Fatalf("只从 main.go 认出 %d 个开关，解析恐怕失效了", len(flags))
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("读 README: %v", err)
	}
	text := string(readme)
	var missing []string
	for _, f := range flags {
		// 必须带边界：纯子串匹配会把 -metrics-removed 里的 -metrics 当成「写了」。
		// 这条是变异验证抓出来的——第一版就是这么假绿的。
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_-])-` + regexp.QuoteMeta(f) + `([^A-Za-z0-9_-]|$)`)
		if !re.MatchString(text) {
			missing = append(missing, "-"+f)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("这些开关在 main.go 里有、README 里没写：%s", strings.Join(missing, "、"))
	}
}

// flagsInMain 从 cmd/dbrover/main.go 取所有 flag 名。
func flagsInMain(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../../cmd/dbrover/main.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 main.go: %v", err)
	}
	seen := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "String", "Bool", "Int", "Int64", "Duration", "Float64":
		default:
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		name, err := strconv.Unquote(lit.Value)
		if err == nil && name != "" {
			seen[name] = true
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
