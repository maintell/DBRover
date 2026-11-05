package webui

import (
	"os"
	"strings"
	"testing"
)

// TestUISessionsPanel 守住会话面板（FR-MON-02）在界面里的接线。
//
// 后端有了端点但界面没入口，就是「能力存在、用户用不到」——这仓库已经踩过两次
// （对象搜索、行级增删）。这里钉住：按钮按能力位显示、点了会读会话、只读连接不摆
// 终止按钮、终止前必须先弹确认。
func TestUISessionsPanel(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	h, src := string(html), string(js)

	if !strings.Contains(h, `id="btn-sessions"`) {
		t.Fatal("index.html 里没有会话按钮")
	}
	for _, want := range []string{
		"sessions: '/api/v1/connections/:id/monitor/sessions'",
		"killSession: '/api/v1/connections/:id/monitor/sessions/:sid/kill'",
		// 按钮按能力位显示：SQLite 没有会话概念，不该出现。
		"$('btn-sessions').hidden = !caps.session_mgmt",
		"$('btn-sessions').onclick = () => showSessions()",
		"async function showSessions()",
		"async function killSession(it)",
		// 终止是不可逆写操作：必须弹确认，而且只读连接不给入口。
		"const ok = await dialog('终止会话'",
		// 确认框必须带上「它正在跑什么」，并把语句转义后再拼进 HTML（docs/09 验收 6）。
		"it.query || ''",
		"function escapeText(s)",
		"escapeText(q.length > 400 ? q.slice(0, 400) + ' …' : q)",
		"!!caps.kill_session && state.current.access_mode !== 'read-only'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("会话面板缺少 %q", want)
		}
	}
}
