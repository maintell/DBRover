package ci

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 这三条门禁守的是同一件事：文档里「点得开、找得到」的那些引用不能悄悄失效。
//
// 验收清单的说服力全靠「这条有 TestX 兜着」「细节见 ADR-00NN」「跑 scripts/accept/x.ps1」。
// 测试改名、ADR 重编号、脚本换名之后文档不会自己更新，也不会产生任何编译错误——
// 读者点进去看到 404，才知道那句验收是空的。

var (
	citeTestRe   = regexp.MustCompile(`\bTest[A-Za-z0-9_]{2,}\b`)
	defTestRe    = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	adrLinkRe    = regexp.MustCompile(`\((?:adr/)?(\d{4}-[a-z0-9-]+\.md)\)`)
	scriptCiteRe = regexp.MustCompile(`scripts/[a-zA-Z0-9_\-/]+\.ps1`)
)

// repoRoot 返回仓库根。go test 的工作目录是包目录，所以往上两级。
func repoRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("找不到仓库根（%s 下没有 go.mod）: %v", root, err)
	}
	return root
}

// docFiles 列出 docs 下所有 Markdown。
func docFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 docs: %v", err)
	}
	if len(out) < 10 {
		t.Fatalf("只找到 %d 份文档，扫描恐怕失效了", len(out))
	}
	return out
}

// definedTests 收集仓库里所有测试函数名。
func definedTests(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不动就跳过，不让一个权限问题挡住整条门禁
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for _, m := range defTestRe.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历仓库: %v", err)
	}
	if len(out) < 200 {
		t.Fatalf("只认出 %d 个测试函数，解析恐怕失效了", len(out))
	}
	return out
}

// TestDocTestCitationsExist 让「文档引用了一个不存在的测试」变红。
//
// 判据是「同名或以它为前缀」：文档里常写 `TestBulkPlan` 指一整族
// `TestBulkPlan*`，那种写法是有效的指路，不该被判成悬空。整族被改名时仍然会红。
func TestDocTestCitationsExist(t *testing.T) {
	root := repoRoot(t)
	defined := definedTests(t, root)
	names := make([]string, 0, len(defined))
	for n := range defined {
		names = append(names, n)
	}

	var missing []string
	seen := map[string]bool{}
	for _, doc := range docFiles(t, root) {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("读 %s: %v", doc, err)
		}
		for _, cite := range citeTestRe.FindAllString(string(b), -1) {
			key := filepath.Base(doc) + "|" + cite
			if seen[key] {
				continue
			}
			seen[key] = true
			ok := false
			for _, d := range names {
				if d == cite || strings.HasPrefix(d, cite) {
					ok = true
					break
				}
			}
			if !ok {
				missing = append(missing, filepath.Base(doc)+" 引用 "+cite)
			}
		}
	}
	if len(seen) < 100 {
		t.Fatalf("只扫到 %d 处测试引用，扫描恐怕失效了", len(seen))
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("文档引用了不存在的测试：%s（改名或删除后要同步文档）", m)
	}
}

// TestDocAdrLinksResolve 让「文档里点不开的 ADR 链接」变红。
func TestDocAdrLinksResolve(t *testing.T) {
	root := repoRoot(t)
	var missing []string
	for _, doc := range docFiles(t, root) {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("读 %s: %v", doc, err)
		}
		for _, m := range adrLinkRe.FindAllStringSubmatch(string(b), -1) {
			p := filepath.Join(root, "docs", "adr", m[1])
			if _, serr := os.Stat(p); serr != nil {
				missing = append(missing, filepath.Base(doc)+" -> "+m[1])
			}
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("ADR 链接指向不存在的文件：%s", m)
	}
}

// TestDocScriptPathsExist 让「文档里让人跑的脚本不存在」变红。
func TestDocScriptPathsExist(t *testing.T) {
	root := repoRoot(t)
	var missing []string
	count := 0
	for _, doc := range docFiles(t, root) {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("读 %s: %v", doc, err)
		}
		for _, cite := range scriptCiteRe.FindAllString(string(b), -1) {
			count++
			if _, serr := os.Stat(filepath.Join(root, filepath.FromSlash(cite))); serr != nil {
				missing = append(missing, filepath.Base(doc)+" -> "+cite)
			}
		}
	}
	if count == 0 {
		t.Fatal("没在文档里扫到任何 scripts/*.ps1 引用，扫描恐怕失效了")
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("文档指向不存在的脚本：%s", m)
	}
}
