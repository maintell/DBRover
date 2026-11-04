package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestEstimateTokens 锁住估算规则：CJK 一个字算 1 token，其余 4 字符算 1。
//
// 这条规则决定了压缩在什么时候触发，估错了要么频繁压（丢信息）、要么根本
// 压不下去（撞窗口上限）。
func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"空串", "", 0},
		{"纯 ASCII 4 字符", "abcd", 1},
		{"纯 ASCII 5 字符向上取整", "abcde", 2},
		{"纯中文", "你好世界", 4},
		{"中英混合", "select 用户", 4}, // select=6 字符→2，用户=2
		{"中文标点也算宽字符", "好，", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := estimateTokens(c.in); got != c.want {
				t.Errorf("estimateTokens(%q) = %d，期望 %d", c.in, got, c.want)
			}
		})
	}
}

// TestCompactByTokensAtThreshold 锁住「窗口的 85% 才压」这条口径。
func TestCompactByTokensAtThreshold(t *testing.T) {
	// 每段工具结果是 900 个 ASCII 字符 = 225 token。
	body := strings.Repeat("x", 900)
	msgs := []Message{
		{Role: "user", Content: "开始"},
		{Role: "tool", Name: "query.execute", Content: body},
		{Role: "assistant", Content: "看了看"},
		{Role: "tool", Name: "query.execute", Content: body},
		{Role: "tool", Name: "query.execute", Content: body},
	}
	const threshold = 850 // 窗口 1000 的 85%

	// 三段共 675 token，未过阈值：不该压。
	if _, n := compactHistoryByTokens(msgs, threshold); n != 0 {
		t.Fatalf("未到阈值不该压缩，实际压了 %d 条", n)
	}

	// 再加一段（900 token）越过阈值：要压，且不动最新一条。
	more := append(append([]Message{}, msgs...),
		Message{Role: "tool", Name: "query.execute", Content: body})
	out, n := compactHistoryByTokens(more, threshold)
	if n == 0 {
		t.Fatal("超过 85% 阈值时应触发压缩")
	}
	if !strings.Contains(out[1].Content, "已压缩") {
		t.Error("最早的工具结果应被压缩")
	}
	if out[len(out)-1].Content != body {
		t.Error("最新一条工具结果不该被压缩")
	}
	if !utf8.ValidString(out[1].Content) {
		t.Error("压缩结果必须是合法 UTF-8")
	}
}

// TestCompactKeepsValidUTF8 锁住「按字符截断而不是按字节」。
//
// 早先按字节切 200，中文堆得多时会切出半个字——那条消息变成非法 UTF-8，
// 发给模型只会得到乱码或 400。
func TestCompactKeepsValidUTF8(t *testing.T) {
	body := strings.Repeat("数据", 300) // 600 个字符、1800 字节
	msgs := []Message{
		{Role: "tool", Name: "query.execute", Content: body},
		{Role: "tool", Name: "query.execute", Content: body},
	}
	out, n := compactHistoryByTokens(msgs, 100) // 逼它压
	if n == 0 {
		t.Fatal("应当发生压缩")
	}
	for i, m := range out {
		if !utf8.ValidString(m.Content) {
			t.Fatalf("第 %d 条压缩后不是合法 UTF-8", i)
		}
	}
}

// TestRuntimeCompactPrefersWindow 锁住两条口径的切换：
// 配了上下文窗口就按估算 token 的 85% 压；没配就沿用字符预算，行为不变。
func TestRuntimeCompactPrefersWindow(t *testing.T) {
	body := strings.Repeat("x", 900) // 225 token / 900 字符
	msgs := []Message{
		{Role: "tool", Name: "query.execute", Content: body},
		{Role: "tool", Name: "query.execute", Content: body},
		{Role: "tool", Name: "query.execute", Content: body},
		{Role: "tool", Name: "query.execute", Content: body},
	}

	// 窗口 1000 → 阈值 850；四段共 900 token，越过，应当压缩。
	withWindow := (&Runtime{contextTokens: 1000}).compact(msgs)
	if withWindow[0].Content == body {
		t.Fatal("给了上下文窗口就该按 85% 触发压缩")
	}

	// 没给窗口：字符预算 24000，四段共 3600 字符，不该压缩。
	withoutWindow := (&Runtime{}).compact(msgs)
	if withoutWindow[0].Content != body {
		t.Fatal("没给窗口时应沿用字符预算，不该压缩")
	}
}
