package webui

import (
	"os"
	"strings"
	"testing"
)

// TestSlowQueryTimerWiring 钉住「慢查询报时」这条链路确实连着。
//
// 为什么要测：docs/05 §6 写着「超过 3 秒显示已运行时长并可取消、超过 30 秒问
// 一次要不要继续等」。这三件事分散在计时器、状态栏和弹窗里，任何一处被删掉，
// 界面都只是「不报时了」——不报错、不明显，只有真跑一条大查询才发现。
//
// 断言只到源码形状这一层：webui 没有 JS 运行时测试，能挡住的是「实现被删掉」，
// 挡不住「逻辑写错」。真机行为另由内置浏览器实测（见 CHANGELOG）。
func TestSlowQueryTimerWiring(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	mustHave := []struct{ name, needle string }{
		{"3 秒阈值", "const SLOW_QUERY_SECONDS = 3;"},
		{"30 秒提示阈值", "const PROMPT_SECONDS = 30;"},
		{"状态栏报时", "（可取消）"},
		{"30 秒弹窗", "查询已运行 30 秒"},
		{"取消复用同一条路", "async function cancelRunning()"},
		{"runSQL 里真的启动计时", "const stopTimer = startQueryTimer("},
		{"收尾一定要停表", "stopTimer();"},
		{"结束后收起已失效的弹窗", "PROMPT_TITLE"},
		{"弹窗之后才判断要不要真的取消", "if (!keep && stillRunning) await cancelRunning();"},
	}
	for _, c := range mustHave {
		if !strings.Contains(src, c.needle) {
			t.Errorf("app.js 里缺少「%s」（找不到 %q）：慢查询报时链路断了", c.name, c.needle)
		}
	}

	// 计时器必须在 finally 里停：异常路径上停不下来会留下一个永远在跑的
	// setInterval，把状态栏刷成「执行中」而实际早就结束了。
	// 先置 stillRunning=false 再停表：顺序反过来的话，收尾那一瞬间弹窗仍可能
	// 认为语句在跑，从而发出一次无意义的取消。
	if !strings.Contains(src, "} finally {\n    stillRunning = false;\n    stopTimer();") {
		t.Error("finally 里必须先 stillRunning = false 再 stopTimer()")
	}
}
