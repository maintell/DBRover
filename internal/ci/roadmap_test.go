package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var (
	adrCountRe = regexp.MustCompile(`(\d+) 份 ADR`)
	fourDigit  = regexp.MustCompile(`\b0\d{3}\b`)
)

// TestRoadmapListsEveryAdr 守住 docs/13 §1.5 里那份 ADR 清单。
//
// 那行写着「N 份 ADR（0001 … / 0015 …）」，是全项目决策的索引。三种腐烂都不会
// 产生编译错误：新增 ADR 却没登记（读路线图的人看不到它）、列了一个不存在的编号
// （点进去 404）、份数写死之后再也不更新（0015 加进来时它就还写着 14）。
func TestRoadmapListsEveryAdr(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "docs", "adr"))
	if err != nil {
		t.Fatalf("读 docs/adr: %v", err)
	}
	actual := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || len(name) < 4 {
			continue
		}
		actual[name[:4]] = true
	}
	if len(actual) < 10 {
		t.Fatalf("只认出 %d 份 ADR，扫描恐怕失效了", len(actual))
	}

	b, err := os.ReadFile(filepath.Join(root, "docs", "13-roadmap.md"))
	if err != nil {
		t.Fatalf("读 docs/13: %v", err)
	}
	text := string(b)

	// 1) 份数要对得上。
	m := adrCountRe.FindStringSubmatch(text)
	if m == nil {
		t.Fatal("没在 docs/13 里找到「N 份 ADR」这句话，扫描恐怕失效了")
	}
	if n, _ := strconv.Atoi(m[1]); n != len(actual) {
		t.Errorf("docs/13 写着 %d 份 ADR，实际有 %d 份（新增 ADR 后要更新 M0 行）", n, len(actual))
	}

	// 2) 每份真实 ADR 都要在路线图里出现。
	var missing []string
	for num := range actual {
		if !strings.Contains(text, num) {
			missing = append(missing, num)
		}
	}
	sort.Strings(missing)
	for _, num := range missing {
		t.Errorf("ADR %s 没有登记进 docs/13 §1.5 的 M0 行", num)
	}

	// 3) 清单里列出的编号必须真实存在（只在 M0 那一行取，别把其它数字当编号）。
	var m0 string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "ADR（") {
			m0 = line
			break
		}
	}
	if m0 == "" {
		t.Fatal("没找到 M0 那行 ADR 清单，扫描恐怕失效了")
	}
	for _, num := range fourDigit.FindAllString(m0, -1) {
		if !actual[num] {
			t.Errorf("docs/13 的 ADR 清单里写了 %s，但 docs/adr 下没有这份文件", num)
		}
	}
}
