package webui

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestExportFormatsOfferedMatchBackend 让「后端支持但界面没入口」变红。
//
// 这次就是这种情况：API 早就支持 format=sql，界面的导出下拉只有 CSV / NDJSON，
// 想拿一份 SQL 脚本只能去敲 curl——功能在、没人找得到，等于没有。
//
// 判据从上位（internal/service/export.go 的常量）取，不从界面自己声明，
// 否则两边各写一份清单，漏掉也不会红。
func TestExportFormatsOfferedMatchBackend(t *testing.T) {
	backend, err := os.ReadFile("../service/export.go")
	if err != nil {
		t.Fatal(err)
	}
	ui, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	uiSrc := string(ui)

	re := regexp.MustCompile(`Export\w+ ExportFormat = "(\w+)"`)
	matches := re.FindAllStringSubmatch(string(backend), -1)
	if len(matches) < 3 {
		t.Fatalf("只从 export.go 里认出 %d 种格式，正则恐怕失效了", len(matches))
	}
	for _, m := range matches {
		format := m[1]
		if !strings.Contains(uiSrc, "'"+format+"'") {
			t.Errorf("后端支持 format=%s，界面的导出下拉里却没有——用户只能去敲 curl", format)
		}
	}
}
