package api_test

import (
	"net/http"
	"testing"
)

func TestChangePlanLifecycle(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	if code := execApproved(t, ts, id, "CREATE TABLE c (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表 status = %d", code)
	}
	if code := execApproved(t, ts, id, "INSERT INTO c (id,v) VALUES (1,'a'),(2,'b')"); code != http.StatusOK {
		t.Fatalf("插数据 status = %d", code)
	}

	// 1. 生成预案
	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/changes/plan", map[string]any{
		"table": map[string]any{"kind": "table", "name": "c"},
		"changes": []any{
			map[string]any{
				"op":     "update",
				"key":    map[string]any{"id": 1},
				"before": map[string]any{"id": 1, "v": "a"},
				"after":  map[string]any{"v": "a2"},
			},
		},
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("plan status = %d, body=%v", code, body)
	}
	pid, _ := body["id"].(string)
	if pid == "" {
		t.Fatal("响应缺少预案 id")
	}
	stmts, _ := body["statements"].([]any)
	if len(stmts) != 1 {
		t.Fatalf("语句数 = %d", len(stmts))
	}

	// 2. 执行
	code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply", map[string]any{}, true)
	if code != http.StatusOK {
		t.Fatalf("apply status = %d, body=%v", code, body)
	}

	// 3. 数据已变
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "SELECT v FROM c WHERE id = 1",
	}, true)
	if code != http.StatusOK {
		t.Fatalf("查询 status = %d", code)
	}
	results, _ := body["results"].([]any)
	rows, _ := results[0].(map[string]any)["rows"].([]any)
	if len(rows) != 1 || rows[0].([]any)[0] != "a2" {
		t.Errorf("数据未生效: %v", rows)
	}

	// 4. 预案用后作废
	code, _ = do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply", map[string]any{}, true)
	if code != http.StatusNotFound {
		t.Errorf("重复应用 status = %d, 期望 404", code)
	}
}

// TestChangePlanConflictReturns409 是乐观并发的端到端回归。
func TestChangePlanConflictReturns409(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	if code := execApproved(t, ts, id, "CREATE TABLE c (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表 status = %d", code)
	}
	if code := execApproved(t, ts, id, "INSERT INTO c (id,v) VALUES (1,'a')"); code != http.StatusOK {
		t.Fatalf("插数据 status = %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/changes/plan", map[string]any{
		"table": map[string]any{"kind": "table", "name": "c"},
		"changes": []any{
			map[string]any{
				"op":     "update",
				"key":    map[string]any{"id": 1},
				"before": map[string]any{"id": 1, "v": "a"},
				"after":  map[string]any{"v": "mine"},
			},
		},
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("plan status = %d", code)
	}
	pid, _ := body["id"].(string)

	// 别人先改
	if code := execApproved(t, ts, id, "UPDATE c SET v='theirs' WHERE id=1"); code != http.StatusOK {
		t.Fatalf("外部更新 status = %d", code)
	}

	code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply", map[string]any{}, true)
	if code != http.StatusConflict {
		t.Fatalf("冲突 status = %d, 期望 409, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "CHANGE_ROW_CONFLICT" {
		t.Errorf("code = %v, 期望 CHANGE_ROW_CONFLICT", errObj["code"])
	}
}

// TestChangePlanRejectsNoPrimaryKey 保证无主键表在预案阶段就被拦住。
func TestChangePlanRejectsNoPrimaryKey(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	if code := execApproved(t, ts, id, "CREATE TABLE np (a TEXT)"); code != http.StatusOK {
		t.Fatalf("建表 status = %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/changes/plan", map[string]any{
		"table": map[string]any{"kind": "table", "name": "np"},
		"changes": []any{
			map[string]any{"op": "delete", "key": map[string]any{"a": "x"}},
		},
	}, true)
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, 期望 501, body=%v", code, body)
	}
}
