package api

import (
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/service"
)

// TestHealthMarkdownEscapesAndLabels 钉住导出的两个要害：
// 级别是中文（贴进工单能直接读），以及表格里的竖线/换行必须转义
// （一个语句里带 | 就能把整张表拆坏）。
func TestHealthMarkdownEscapesAndLabels(t *testing.T) {
	md := healthMarkdown(&service.HealthReport{
		Level: "warn",
		Items: []service.HealthItem{
			{Name: "锁等待", Level: "warn", Detail: "2 条等待 | 最长 7 秒"},
			{Name: "慢查询", Level: "ok", Detail: "第一行\n第二行"},
		},
	})
	for _, want := range []string{"### 数据库体检报告", "整体：**注意**", "| 项目 | 级别 | 结论 |"} {
		if !strings.Contains(md, want) {
			t.Errorf("导出缺少 %q:\n%s", want, md)
		}
	}
	if !strings.Contains(md, "2 条等待 \\| 最长 7 秒") {
		t.Errorf("竖线没有转义:\n%s", md)
	}
	if strings.Contains(md, "第一行\n第二行") {
		t.Errorf("换行没有替换成空格（会把表格拆断）:\n%s", md)
	}
	if !strings.Contains(md, "| 锁等待 | 注意 |") {
		t.Errorf("级别应写成中文:\n%s", md)
	}
}
