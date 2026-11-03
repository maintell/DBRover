package api_test

import (
	"net/http"
	"strings"
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

// TestSchemaPlanAddForeignKeyNeedsCapability 验证接口层：
// SQLite 不支持用 ALTER TABLE 加外键，必须明确拒绝而不是报「未知动作」。
// 真机上的成功路径由 service 层的 TestDDLAddForeignKeyOnRealEngines 覆盖。
func TestSchemaPlanAddForeignKeyNeedsCapability(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "test", "read-write")
	for _, q := range []string{
		"CREATE TABLE fk_p (id INTEGER PRIMARY KEY)",
		"CREATE TABLE fk_c (id INTEGER PRIMARY KEY, pid INTEGER)",
	} {
		if code := execApproved(t, ts, id, q); code != http.StatusOK {
			t.Fatalf("准备表失败 (%s): %d", q, code)
		}
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/schema/plan", map[string]any{
		"table":  map[string]any{"kind": "table", "name": "fk_c"},
		"action": "add_foreign_key",
		"foreign_key": map[string]any{
			"columns": []string{"pid"}, "ref_table": "fk_p", "ref_columns": []string{"id"},
		},
	}, true)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, 期望 400, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "不支持") {
		t.Errorf("应说清是引擎不支持, 实际 %q", msg)
	}
	if strings.Contains(msg, "不支持的动作") {
		t.Errorf("动作已被识别，不该落到「未知动作」分支: %q", msg)
	}
}

// TestSchemaPlanDropTableBlockedByDependency 验证接口层把依赖阻断映射成 409：
// 用户需要据此知道「先删谁」，而不是拿到一句 SCHEMA_PLAN_FAILED。
func TestSchemaPlanDropTableBlockedByDependency(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "test", "read-write")
	for _, q := range []string{
		"CREATE TABLE dep_t (id INTEGER PRIMARY KEY)",
		"CREATE VIEW dep_v AS SELECT id FROM dep_t",
	} {
		if code := execApproved(t, ts, id, q); code != http.StatusOK {
			t.Fatalf("准备 %q 失败: %d", q, code)
		}
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/schema/plan", map[string]any{
		"table":  map[string]any{"kind": "table", "name": "dep_t"},
		"action": "drop_table",
	}, true)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, 期望 409, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "DEPENDENCY_BLOCKED" {
		t.Errorf("code = %v, 期望 DEPENDENCY_BLOCKED", errObj["code"])
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "dep_v") {
		t.Errorf("阻断信息应指出引用方: %q", msg)
	}
	if errObj["hint"] == "" {
		t.Error("必须给出「先删谁」的提示")
	}
}
