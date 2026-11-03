package api_test

import (
	"net/http"
	"path/filepath"
	"testing"
)

// TestCreateConnectionRejectsBadTimeZone 验证时区名拼错时返回 400。
//
// 静默退回 UTC 才是更糟的选择：用户以为自己设了 Asia/Shanghai，
// 结果看到的是 UTC，两边会对着差 8 小时的数据互相怀疑。
func TestCreateConnectionRejectsBadTimeZone(t *testing.T) {
	_, ts, dir := newServer(t)

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections", map[string]any{
		"name": "tz", "engine": "sqlite",
		"host":        filepath.Join(dir, "tz.db"),
		"environment": "test", "access_mode": "read-write",
		"display_timezone": "Asia/Shanghia",
	}, true)
	if code != http.StatusBadRequest {
		t.Fatalf("拼错的时区应 400, 实际 %d, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "REQUEST_INVALID" {
		t.Errorf("code = %v", errObj["code"])
	}

	// 合法值应放行，并在连接信息里回显。
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections", map[string]any{
		"name": "tz2", "engine": "sqlite",
		"host":        filepath.Join(dir, "tz2.db"),
		"environment": "test", "access_mode": "read-write",
		"display_timezone": "Asia/Shanghai",
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("合法时区应 201, 实际 %d, body=%v", code, body)
	}
	if body["display_timezone"] != "Asia/Shanghai" {
		t.Errorf("响应应回显展示时区, 实际 %v", body["display_timezone"])
	}
}
