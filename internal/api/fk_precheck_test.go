package api_test

import (
	"net/http"
	"testing"
)

// TestFKPrecheckEndpoint 验证外键预检端点：
// 有孤儿数据 → orphans>0 且 blocked，并给样本；干净数据 → 不阻断。
func TestFKPrecheckEndpoint(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	for _, q := range []string{
		"CREATE TABLE parent (id INTEGER PRIMARY KEY, code TEXT NOT NULL)",
		"CREATE TABLE child (cid INTEGER PRIMARY KEY, pid INTEGER)",
		"INSERT INTO parent (id, code) VALUES (1, 'A')",
		"INSERT INTO child (cid, pid) VALUES (10, 1), (11, 99)",
	} {
		if code := execApproved(t, ts, id, q); code != http.StatusOK {
			t.Fatalf("准备数据失败 (%s): %d", q, code)
		}
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/fk/precheck", map[string]any{
		"child":       map[string]any{"kind": "table", "name": "child"},
		"parent":      map[string]any{"kind": "table", "name": "parent"},
		"columns":     []string{"pid"},
		"ref_columns": []string{"id"},
	}, true)
	if code != http.StatusOK {
		t.Fatalf("precheck status = %d, body=%v", code, body)
	}
	if body["orphans"] != float64(1) {
		t.Errorf("orphans = %v, 期望 1", body["orphans"])
	}
	if body["blocked"] != true {
		t.Error("有孤儿数据应 blocked=true")
	}
	samples, _ := body["samples"].([]any)
	if len(samples) != 1 {
		t.Errorf("样本数 = %d, 期望 1", len(samples))
	}

	// 删掉孤儿行后应放行。
	if code := execApproved(t, ts, id, "DELETE FROM child WHERE pid = 99"); code != http.StatusOK {
		t.Fatalf("删孤儿失败: %d", code)
	}
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/fk/precheck", map[string]any{
		"child":       map[string]any{"kind": "table", "name": "child"},
		"parent":      map[string]any{"kind": "table", "name": "parent"},
		"columns":     []string{"pid"},
		"ref_columns": []string{"id"},
	}, true)
	if code != http.StatusOK {
		t.Fatalf("precheck status = %d", code)
	}
	if body["blocked"] != false {
		t.Errorf("清理后不该阻断, reason=%v", body["reason"])
	}
	if body["parent_unique"] != true {
		t.Error("父表 id 是主键，parent_unique 应为 true")
	}
}

// TestFKPrecheckEndpointRejectsBadInput 覆盖参数校验。
func TestFKPrecheckEndpointRejectsBadInput(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE parent2 (id INTEGER PRIMARY KEY)"); code != http.StatusOK {
		t.Fatalf("建表失败: %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/fk/precheck", map[string]any{
		"child":       map[string]any{"kind": "table", "name": "parent2"},
		"parent":      map[string]any{"kind": "table", "name": "parent2"},
		"columns":     []string{"id"},
		"ref_columns": []string{"id", "id"},
	}, true)
	if code != http.StatusBadRequest {
		t.Errorf("列数不一致应 400, 实际 %d, body=%v", code, body)
	}
}
