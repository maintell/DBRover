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
	sectionCiteRe = regexp.MustCompile(`docs/(\d{2})[^\n]{0,12}?§\s*(\d+(?:\.\d+)?)`)
	sectionHeadRe = regexp.MustCompile(`(?m)^#{2,4}\s+(\d+(?:\.\d+)?)`)
)

// TestDocSectionRefsResolve 守住跨文档的章节引用（docs/07 §8.2 这类）。
//
// 章节号会随着整理而变（合并、插入、把 §8.2 拆成 §8.2 / §8.3）。引用方不会自己
// 更新，读者点过去发现那一节讲的是别的事——比 404 更难发现。
func TestDocSectionRefsResolve(t *testing.T) {
	root := repoRoot(t)
	docsDir := filepath.Join(root, "docs")

	// 每份文档的小节号。
	heads := map[string]map[string]bool{}
	files, err := os.ReadDir(docsDir)
	if err != nil {
		t.Fatalf("读 docs: %v", err)
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(docsDir, f.Name()))
		if rerr != nil {
			t.Fatalf("读 %s: %v", f.Name(), rerr)
		}
		set := map[string]bool{}
		for _, m := range sectionHeadRe.FindAllStringSubmatch(string(b), -1) {
			set[m[1]] = true
		}
		heads[f.Name()] = set
	}

	var missing []string
	seen := 0
	err = filepath.WalkDir(docsDir, func(p string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for _, m := range sectionCiteRe.FindAllStringSubmatch(string(b), -1) {
			seen++
			num, sec := m[1], m[2]
			target := ""
			for name := range heads {
				if strings.HasPrefix(name, num+"-") {
					target = name
				}
			}
			if target == "" {
				missing = append(missing, filepath.Base(p)+" -> docs/"+num+"（不存在）")
				continue
			}
			if !heads[target][sec] {
				missing = append(missing, filepath.Base(p)+" -> "+target+" §"+sec)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 docs: %v", err)
	}
	if seen < 10 {
		t.Fatalf("只扫到 %d 处章节引用，扫描恐怕失效了", seen)
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("章节引用失效：%s（那一节可能被改名或合并了）", m)
	}
}
