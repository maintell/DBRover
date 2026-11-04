package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestCompleteReturnsPromptlyOnCancel 锁住「取消会让这次补全立刻返回」。
//
// 这是 abort / 单轮时限所依赖的唯一契约。用「上游永不回应」的假服务做判据：
// 只要 Complete 在取消后 1 秒内返回，就说明它没有在等一条永远不会来的响应。
//
// 刻意**不**断言「服务端的 r.Context() 会被取消」：本机（Windows + 本地
// httptest）实测裸 net/http 客户端取消后，服务端 handler 的 ctx 在数秒内都不会
// 触发——那是平台行为，不是我们的代码。拿它当判据会得到一个永远红、且与产品
// 无关的测试；判断「取消有没有生效」要站在调用方这一侧。
func TestCompleteReturnsPromptlyOnCancel(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		// 永不主动回应：只有连接层面被断开才结束。
		<-release
	}))
	defer ts.Close()
	defer close(release)

	o, err := NewOpenAI(OpenAIConfig{BaseURL: ts.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	type out struct {
		err error
	}
	done := make(chan out, 1)
	go func() {
		_, cerr := o.Complete(ctx, Request{Messages: []Message{{Role: "user", Content: "hi"}}})
		done <- out{err: cerr}
	}()

	<-entered
	start := time.Now()
	cancel()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("取消之后不该成功")
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("取消后 %v 才返回：太慢了，运行时会被挂住", d)
		}
	case <-time.After(time.Second):
		t.Fatal("取消后 1 秒仍未返回：Complete 没有把 ctx 传下去")
	}
}
