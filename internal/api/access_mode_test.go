package api_test

import (
	"net/http"
	"path/filepath"
	"testing"
)

// TestCreateConnectionRejectsInvalidAccessMode 是安全回归：
// 曾把 access_mode 直接转成类型不做校验，`read_only` 这种拼写会被当成
// 未知模式，导致只读守卫与驱动只读模式同时失效。
func TestCreateConnectionRejectsInvalidAccessMode(t *testing.T) {
	_, ts, dir := newServer(t)

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections", map[string]any{
		"name":        "sneaky",
		"engine":      "sqlite",
		"host":        filepath.Join(dir, "sneaky.db"),
		"environment": "production",
		"access_mode": "read_only",
	}, true)

	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, 期望 400, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "REQUEST_INVALID" {
		t.Errorf("code = %v, 期望 REQUEST_INVALID", errObj["code"])
	}
}

// TestEmptyAccessModeFallsBackToEnvironment 保证省略时按环境取默认值。
func TestEmptyAccessModeFallsBackToEnvironment(t *testing.T) {
	_, ts, dir := newServer(t)
	path := filepath.Join(dir, "prod-default.db")

	// 先用读写方式把文件建出来：只读打开要求文件已存在。
	code, body := do(t, ts, http.MethodPost, "/api/v1/connections", map[string]any{
		"name":        "seed",
		"engine":      "sqlite",
		"host":        path,
		"environment": "local",
		"access_mode": "read-write",
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("建种子连接 status = %d, body=%v", code, body)
	}
	seedID, _ := body["id"].(string)
	do(t, ts, http.MethodDelete, "/api/v1/connections/"+seedID, nil, true)

	code, body = do(t, ts, http.MethodPost, "/api/v1/connections", map[string]any{
		"name":        "prod-default",
		"engine":      "sqlite",
		"host":        path,
		"environment": "production",
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, body=%v", code, body)
	}
	if body["access_mode"] != "read-only" {
		t.Errorf("access_mode = %v, 期望 read-only（生产环境默认只读）", body["access_mode"])
	}
}
