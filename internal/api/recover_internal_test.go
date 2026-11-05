package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPanicBecomesInternalError 锁住 docs/12 §4 的 INTERNAL：处理器 panic 必须是
// 结构化 500，而不是「空回复 + 连接被掐断」。
//
// 这条以前根本没有产出路径——错误码表里写着 INTERNAL，代码里一次都不出现。
// 用真处理器（一个会 panic 的 HandlerFunc）跑，不 mock writeError：要验证的正是
// 「状态码、错误体形状、request_id 都在」这三件客户端依赖的东西。
func TestPanicBecomesInternalError(t *testing.T) {
	s := &Server{}
	h := s.recoverPanic(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Category  string `json:"category"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是合法 JSON（%v）：%s", err, rec.Body.String())
	}
	if body.Error.Code != "INTERNAL" {
		t.Errorf("错误码 = %q，期望 INTERNAL", body.Error.Code)
	}
	if body.Error.Category != "internal" {
		t.Errorf("类别 = %q，期望 internal", body.Error.Category)
	}
	if body.Error.RequestID == "" {
		t.Error("必须带 request_id：没有它，用户报障时我们对不上任何一行日志")
	}
}

// TestRecoverKeepsFlusher 是反向断言：包一层之后流式端点仍然拿得到 Flusher。
//
// 少了这条，NDJSON（执行进度、Agent 流式）会静默退化成「攒完再发」——功能看起来
// 还在，但「到一条推一条」的意义已经没了，而且没有任何测试会红。
func TestRecoverKeepsFlusher(t *testing.T) {
	s := &Server{}
	var got bool
	h := s.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		got = ok
		if ok {
			f.Flush()
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/stream", nil))
	if !got {
		t.Fatal("包了 recoverPanic 之后 w 不再实现 http.Flusher，流式端点会静默退化")
	}
}

// TestRecoverAfterHeadersWrittenDoesNotDoubleWrite 锁住「头已经发出去就不再追加错误体」：
// 重复写头会让客户端拿到半截响应，比不写更糟。
func TestRecoverAfterHeadersWrittenDoesNotDoubleWrite(t *testing.T) {
	s := &Server{}
	h := s.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("after write")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/partial", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("已写头之后不该改成别的状态码，实际 %d", rec.Code)
	}
	if rec.Body.String() != "partial" {
		t.Errorf("已发出的内容不该被追加错误体，实际 %q", rec.Body.String())
	}
}
