package api_test

import (
	"net/http"
	"testing"
)

// TestExecErrorsUseErrorModel 验证执行期错误走错误模型，而不是 200 + results[].err。
func TestExecErrorsUseErrorModel(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	// 表不存在
	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "SELECT * FROM definitely_missing",
	}, true)
	if code != http.StatusBadRequest {
		t.Fatalf("表不存在 status = %d, 期望 400, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "SQL_OBJECT_NOT_FOUND" {
		t.Errorf("code = %v, 期望 SQL_OBJECT_NOT_FOUND", errObj["code"])
	}

	// 唯一约束冲突
	if code := execApproved(t, ts, id, "CREATE TABLE u (id INTEGER PRIMARY KEY)"); code != http.StatusOK {
		t.Fatalf("建表 status = %d", code)
	}
	if code := execApproved(t, ts, id, "INSERT INTO u (id) VALUES (1)"); code != http.StatusOK {
		t.Fatalf("首次插入 status = %d", code)
	}
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "INSERT INTO u (id) VALUES (1)",
	}, true)
	if code != http.StatusConflict {
		t.Fatalf("重复插入 status = %d, 期望 409, body=%v", code, body)
	}
	errObj, _ = body["error"].(map[string]any)
	if errObj["code"] != "SQL_CONSTRAINT_VIOLATION" {
		t.Errorf("code = %v, 期望 SQL_CONSTRAINT_VIOLATION", errObj["code"])
	}
}

// TestTruncationIsReported 是「截断被当成全量」的回归。
func TestTruncationIsReported(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	if code := execApproved(t, ts, id, "CREATE TABLE s (id INTEGER PRIMARY KEY)"); code != http.StatusOK {
		t.Fatalf("建表 status = %d", code)
	}
	if code := execApproved(t, ts, id,
		"INSERT INTO s (id) VALUES (1),(2),(3),(4),(5),(6),(7),(8)"); code != http.StatusOK {
		t.Fatalf("插数据 status = %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/query", map[string]any{
		"sql":        "SELECT id FROM s ORDER BY id",
		"max_rows":   3,
		"fetch_size": 10,
	}, true)
	if code != http.StatusOK {
		t.Fatalf("query status = %d, body=%v", code, body)
	}
	cid, _ := body["cursor_id"].(string)

	code, body = do(t, ts, http.MethodGet, "/api/v1/results/"+cid+"/page?limit=100", nil, true)
	if code != http.StatusOK {
		t.Fatalf("page status = %d, body=%v", code, body)
	}
	if body["truncated"] != true {
		t.Errorf("达到 max_rows 时必须标记 truncated=true, 实际 %v", body["truncated"])
	}
}
