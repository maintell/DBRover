package api_test

import (
	"net/http"
	"testing"
)

// TestHealthExposesCapabilities 保证「这个引擎能不能做某事」在界面上拿得到。
//
// 之前 /health 只返回版本、身份与告警，前端只能靠「点一次再报错」推断能力，
// 那正是 ADR-0005 要否掉的做法。
func TestHealthExposesCapabilities(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	code, body := do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/health", nil, true)
	if code != http.StatusOK {
		t.Fatalf("health 状态码 = %d", code)
	}
	caps, ok := body["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("health 响应缺少 capabilities: %v", body)
	}
	if caps["explain"] != true {
		t.Errorf("SQLite 支持执行计划，capabilities.explain 应为 true，实际 %v", caps["explain"])
	}
	if caps["indexes"] != true {
		t.Errorf("SQLite 支持索引，capabilities.indexes 应为 true，实际 %v", caps["indexes"])
	}
}
