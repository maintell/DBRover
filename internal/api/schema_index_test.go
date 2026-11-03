package api_test

import (
	"net/http"
	"testing"
)

// TestSchemaIndexLifecycle 走一遍索引闭环：建 → 出现 → 删 → 消失。
func TestSchemaIndexLifecycle(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE ix (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表失败: %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/schema/plan", map[string]any{
		"table":  map[string]any{"kind": "table", "name": "ix"},
		"action": "create_index",
		"index":  map[string]any{"name": "ix_v", "columns": []string{"v"}},
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("create_index status = %d, body=%v", code, body)
	}
	pid, _ := body["id"].(string)

	code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply", map[string]any{}, true)
	if code != http.StatusOK {
		t.Fatalf("apply status = %d, body=%v", code, body)
	}

	code, body = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/tables/ix/schema", nil, true)
	if code != http.StatusOK {
		t.Fatalf("schema status = %d", code)
	}
	if !hasIndex(body, "ix_v") {
		t.Fatalf("索引没建出来: %v", body["indexes"])
	}

	// 删索引
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/schema/plan", map[string]any{
		"table":  map[string]any{"kind": "table", "name": "ix"},
		"action": "drop_index",
		"index":  map[string]any{"name": "ix_v"},
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("drop_index status = %d, body=%v", code, body)
	}
	pid, _ = body["id"].(string)
	if code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply", map[string]any{}, true); code != http.StatusOK {
		t.Fatalf("drop apply status = %d, body=%v", code, body)
	}
	code, body = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/tables/ix/schema", nil, true)
	if code != http.StatusOK {
		t.Fatalf("schema status = %d", code)
	}
	if hasIndex(body, "ix_v") {
		t.Fatal("索引删除后仍在")
	}
}

// TestSchemaUniqueIndexReturns409 验证重复值会以 409 DUPLICATE_ROWS 返回，
// 而不是等执行时让引擎抛一句无从下手的错。
func TestSchemaUniqueIndexReturns409(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE ix2 (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表失败: %d", code)
	}
	if code := execApproved(t, ts, id, "INSERT INTO ix2 (id,v) VALUES (1,'dup'),(2,'dup')"); code != http.StatusOK {
		t.Fatalf("插数据失败: %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/schema/plan", map[string]any{
		"table":  map[string]any{"kind": "table", "name": "ix2"},
		"action": "create_index",
		"index":  map[string]any{"name": "ux_v", "columns": []string{"v"}, "unique": true},
	}, true)
	if code != http.StatusConflict {
		t.Fatalf("重复值应 409, 实际 %d, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "DUPLICATE_ROWS" {
		t.Errorf("code = %v, 期望 DUPLICATE_ROWS", errObj["code"])
	}
}

func hasIndex(body map[string]any, name string) bool {
	raw, _ := body["indexes"].([]any)
	for _, it := range raw {
		m, _ := it.(map[string]any)
		if n, _ := m["name"].(string); n == name {
			return true
		}
	}
	return false
}
