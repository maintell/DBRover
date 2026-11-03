package api_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// TestCompareEndpoint 验证结构比对端点：两侧不同 → 报出差异并给增量脚本；
// 同一连接跟自己比 → 无差异。
func TestCompareEndpoint(t *testing.T) {
	_, ts, dir := newServer(t)

	// 左侧：a(id, v TEXT)
	leftID := createConnAt(t, ts, filepath.Join(dir, "left.db"), "left", "read-write")
	if code := execApproved(t, ts, leftID, "CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("左侧建表失败: %d", code)
	}

	// 右侧：a(id, v TEXT, extra TEXT) + b(id)
	rightID := createConnAt(t, ts, filepath.Join(dir, "right.db"), "right", "read-write")
	for _, q := range []string{
		"CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT, extra TEXT)",
		"CREATE TABLE b (id INTEGER PRIMARY KEY)",
	} {
		if code := execApproved(t, ts, rightID, q); code != http.StatusOK {
			t.Fatalf("右侧建表失败 (%s): %d", q, code)
		}
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/compare", map[string]any{
		"left": leftID, "right": rightID,
	}, true)
	if code != http.StatusOK {
		t.Fatalf("compare status = %d, body=%v", code, body)
	}

	tables, _ := body["tables"].([]any)
	if len(tables) != 2 {
		t.Fatalf("差异表数 = %d, 期望 2（a 有差异 + b 只在右侧）", len(tables))
	}
	upgrade, _ := body["upgrade"].([]any)
	if len(upgrade) == 0 {
		t.Fatal("应生成升级脚本")
	}
	review, _ := body["need_review"].([]any)
	if len(review) == 0 {
		t.Error("应列出需要人工决策的点")
	}

	// 自己跟自己比：无差异。
	code, body = do(t, ts, http.MethodPost, "/api/v1/compare", map[string]any{
		"left": leftID, "right": leftID,
	}, true)
	if code != http.StatusOK {
		t.Fatalf("self compare status = %d", code)
	}
	if items, _ := body["tables"].([]any); len(items) != 0 {
		t.Errorf("自比不该有差异, 实际 %d 项", len(items))
	}
}

// TestCompareEndpointErrors 覆盖参数与连接校验。
func TestCompareEndpointErrors(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	code, body := do(t, ts, http.MethodPost, "/api/v1/compare", map[string]any{"left": id}, true)
	if code != http.StatusBadRequest {
		t.Errorf("缺 right 应 400, 实际 %d, body=%v", code, body)
	}

	code, _ = do(t, ts, http.MethodPost, "/api/v1/compare", map[string]any{
		"left": id, "right": "conn-not-exist",
	}, true)
	if code != http.StatusNotFound {
		t.Errorf("连接不存在应 404, 实际 %d", code)
	}
}

// createConnAt 在指定文件上建一个连接（比对需要两个独立库）。
func createConnAt(t *testing.T, ts *httptest.Server, host, name, mode string) string {
	t.Helper()
	code, body := do(t, ts, http.MethodPost, "/api/v1/connections", map[string]any{
		"name": name, "engine": "sqlite", "host": host,
		"environment": "local", "access_mode": mode,
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("创建连接失败: %d %v", code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatal("响应缺少 id")
	}
	return id
}
