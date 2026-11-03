package api_test

import (
	"net/http"
	"testing"
)

// TestBulkPlanLifecycle 走一遍条件批量修改：出预案 → 应用 → 值变了。
func TestBulkPlanLifecycle(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE blk (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表失败: %d", code)
	}
	if code := execApproved(t, ts, id, "INSERT INTO blk (id,v) VALUES (1,'a'),(2,'a'),(3,'b')"); code != http.StatusOK {
		t.Fatalf("插数据失败: %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/bulk/plan", map[string]any{
		"table":  map[string]any{"kind": "table", "name": "blk"},
		"set":    map[string]any{"v": "done"},
		"filter": []any{map[string]any{"column": "v", "op": "eq", "value": "a"}},
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("bulk/plan status = %d, body=%v", code, body)
	}
	if body["affected"] != float64(2) {
		t.Errorf("affected = %v, 期望 2", body["affected"])
	}
	samples, _ := body["samples"].([]any)
	if len(samples) != 2 {
		t.Errorf("样本数 = %d, 期望 2", len(samples))
	}
	plan, _ := body["plan"].(map[string]any)
	pid, _ := plan["id"].(string)
	if pid == "" {
		t.Fatal("预案缺少 id")
	}

	if code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply", map[string]any{}, true); code != http.StatusOK {
		t.Fatalf("apply status = %d, body=%v", code, body)
	}

	// 只有 v='a' 的两行被改，v='b' 的那行不变。
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "SELECT COUNT(*) AS n FROM blk WHERE v = 'done'",
	}, true)
	if code != http.StatusOK {
		t.Fatalf("查询失败: %d", code)
	}
	results, _ := body["results"].([]any)
	rows, _ := results[0].(map[string]any)["rows"].([]any)
	if len(rows) != 1 || rows[0].([]any)[0] != float64(2) {
		t.Errorf("被改行数不对: %v", rows)
	}
}

// TestBulkPlanRequiresTableName 验证无条件批量修改必须回填表名（428）。
func TestBulkPlanRequiresTableName(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE blk2 (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表失败: %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/bulk/plan", map[string]any{
		"table": map[string]any{"kind": "table", "name": "blk2"},
		"set":   map[string]any{"v": "all"},
	}, true)
	if code != http.StatusPreconditionRequired {
		t.Fatalf("无条件批量修改应 428, 实际 %d, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "BULK_CONFIRM_TABLE_REQUIRED" {
		t.Errorf("code = %v", errObj["code"])
	}

	// 回填表名后放行。
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/bulk/plan", map[string]any{
		"table":         map[string]any{"kind": "table", "name": "blk2"},
		"set":           map[string]any{"v": "all"},
		"confirm_table": "blk2",
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("回填表名后应 201, 实际 %d, body=%v", code, body)
	}
}
