package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// itemsOf 取出响应里的 items 数组。
func itemsOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	raw, ok := body["items"]
	if !ok {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("items 不是数组：%T", raw)
	}
	return items
}

// hasName 判断 items 里有没有某个名字的对象。
func hasName(items []any, name string) bool {
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if m["name"] == name {
			return true
		}
	}
	return false
}

// errMessage 取错误响应里的 message。
func errMessage(body map[string]any) string {
	e, ok := body["error"].(map[string]any)
	if !ok {
		return ""
	}
	msg, _ := e["message"].(string)
	return msg
}

// TestSearchEndpoint 守住 FR-META-04（对象全局搜索）的 HTTP 出口。
//
// 驱动层的 Meta().Search 三引擎都实现了、也有 drivertest 用例，但一直没有
// HTTP 出口——需求是 P0，实际等于没做。这条把端点钉住：命中、类型过滤、
// 参数校验各一条。
func TestSearchEndpoint(t *testing.T) {
	_, ts, dir := newServer(t)
	connID := createConn(t, ts, dir, "test", "read-write")
	execSQL(t, ts, connID, "CREATE TABLE orders (id INTEGER PRIMARY KEY)")
	execSQL(t, ts, connID, "CREATE TABLE users (id INTEGER PRIMARY KEY)")
	base := "/api/v1/connections/" + connID + "/search"

	// 1) 名称命中，且不该把无关的表带出来。
	code, body := do(t, ts, http.MethodGet, base+"?q=ord", nil, true)
	if code != http.StatusOK {
		t.Fatalf("搜索 = %d %v", code, body)
	}
	items := itemsOf(t, body)
	if !hasName(items, "orders") {
		t.Errorf("应命中 orders：%v", items)
	}
	if hasName(items, "users") {
		t.Errorf("不该带出 users：%v", items)
	}

	// 2) kind 过滤：orders 是表，按 view 过滤要空。
	code, body = do(t, ts, http.MethodGet, base+"?q=ord&kind=view", nil, true)
	if code != http.StatusOK {
		t.Fatalf("带 kind 的搜索 = %d %v", code, body)
	}
	if n := len(itemsOf(t, body)); n != 0 {
		t.Errorf("按 view 过滤后应为空，实际 %d 条", n)
	}

	// 3) 空 q 要说清怎么用，而不是返回全库。
	code, body = do(t, ts, http.MethodGet, base+"?q=", nil, true)
	if code != http.StatusBadRequest {
		t.Fatalf("空 q 应为 400，实际 %d %v", code, body)
	}
	if !strings.Contains(errMessage(body), "q 必填") {
		t.Errorf("错误应说清 q 必填：%v", body)
	}

	// 4) 拼错 kind 要被指名，不能静默忽略——否则用户会以为库里真没有视图。
	code, body = do(t, ts, http.MethodGet, base+"?q=x&kind=nope", nil, true)
	if code != http.StatusBadRequest {
		t.Fatalf("非法 kind 应为 400，实际 %d %v", code, body)
	}
	if !strings.Contains(errMessage(body), "kind 只接受") {
		t.Errorf("错误应列出允许的 kind：%v", body)
	}

	// 5) limit 非法同理。
	code, body = do(t, ts, http.MethodGet, base+"?q=x&limit=abc", nil, true)
	if code != http.StatusBadRequest {
		t.Fatalf("非法 limit 应为 400，实际 %d %v", code, body)
	}
}
