package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUIMetricsPanel 守住性能面板的接线（FR-MON-01）。
//
// 后端把计数读出来之后，界面不画等于没做——这仓库已经踩过「能力有、入口没有」
// 好几次。这里同时钉住两件容易漏的事：按钮按能力位显示，以及**关闭面板后停止
// 采集**（docs/09 验收 10）——判断写在定时器自己身上，任何切视图的路径都不必
// 记得清理。
func TestUIMetricsPanel(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{
		"metrics: '/api/v1/connections/:id/monitor/metrics'",
		"function showMetrics(",
		// 六块数据并发发（验收 1 是 2 秒出首屏，串行等于「最慢的 × 6」）。
		"Promise.allSettled(",
		"const pick = (r) => (r.status === 'fulfilled' ? r.value : null)",
		"function renderMetrics(",
		// 定时器自己判断面板还在不在，不在就停。
		"if (!$('metrics-panel') || !state.current)",
		"clearInterval(metricsTimer)",
		// 结论来自服务端规则，界面上不二次判断。
		"out.verdict",
		// 入口按能力位显示。
		"if ($('btn-metrics')) $('btn-metrics').hidden = !caps.metrics;",
		// 慢查询区（FR-MON-04）：首次采样必须标注「没有增量」，否则用户会把累计值
		// 当成最近这段窗口的量（docs/09 §6.2 点名的坑）。
		"slowQueries: '/api/v1/connections/:id/monitor/slow-queries'",
		"function slowSection(slow)",
		"首次采样，暂无增量",
		"slow.available === false",
		// 表体量（FR-MON-06）：只给体量不给增长。
		"tableSizes: '/api/v1/connections/:id/monitor/tables'",
		"function sizeSection(sizes)",
		"function formatBytes(v)",
		// 体量增长要有历史才算得出来，没有就说「需要更多采样」。
		"需要更多采样",
		"function formatUptime(s)",
		// 锁等待（FR-MON-03）：空列表要写成「没有等待」，别让人分不清是不是没接上。
		"locks: '/api/v1/connections/:id/monitor/locks'",
		"function lockSection(locks)",
		"当前没有锁等待。",
		// 趋势（FR-MON-05）：没开留存时如实说没开，不画平线糊弄。
		"history: '/api/v1/connections/:id/monitor/history'",
		"function historySection(hist)",
		"没有开启指标留存。",
		// 巡检（FR-MON-07）：结论来自服务端规则，界面只负责摆清级别。
		"healthCheck: '/api/v1/connections/:id/monitor/health-check'",
		// 告警（FR-MON-08）：空列表也要说明「只在运行时生效」。
		"alerts: '/api/v1/connections/:id/monitor/alerts'",
		"function alertSection(alerts)",
		"function showHealth()",
		// 巡检报告可导出（验收 12 的后半）。
		"id = 'btn-health-export'",
		"'?format=markdown'",
		"unknown: '未知'",
		// 链而不是平表：从持有者往下展开（FR-MON-03 要的是「谁卡住了谁」）。
		"'持有 #' + id",
		// 按会话计数：同一个会话可能被多个持有者挡着，按边计数会和行数对不上。
		"' 个会话在等）'",
		"在等（'",
		// 新实例上「0 天 0 小时」等于没说：按最大量级给两个单位。
		"return m + ' 分钟'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("性能面板缺少 %q", want)
		}
	}

	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), "id=\"btn-metrics\"") {
		t.Error("index.html 里没有指标按钮（#btn-metrics）")
	}
}
