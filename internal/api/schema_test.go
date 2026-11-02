package api_test

import (
	"net/http"
	"testing"
)

// TestSchemaLifecycle 走一遍结构设计器的闭环：
// 读结构 → 生成加列预案 → 应用 → 结构里出现新列 → 撤销 → 列消失。
func TestSchemaLifecycle(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	if code := execApproved(t, ts, id, "CREATE TABLE st (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表 status = %d", code)
	}

	// 1. 读结构
	code, body := do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/tables/st/schema", nil, true)
	if code != http.StatusOK {
		t.Fatalf("schema status = %d, body=%v", code, body)
	}
	cols, _ := body["columns"].([]any)
	if len(cols) != 2 {
		t.Fatalf("列数 = %d, 期望 2", len(cols))
	}
	if ddl, _ := body["ddl"].(string); ddl == "" {
		t.Error("应返回 DDL 原文")
	}
	if pk, _ := body["primary_key"].([]any); len(pk) != 1 || pk[0] != "id" {
		t.Errorf("primary_key = %v", body["primary_key"])
	}

	// 2. 生成加列预案
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/schema/plan", map[string]any{
		"table":  map[string]any{"kind": "table", "name": "st"},
		"action": "add_column",
		"column": map[string]any{"name": "note", "type": "TEXT", "nullable": true},
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("schema/plan status = %d, body=%v", code, body)
	}
	if body["ddl"] != true {
		t.Error("预案应标记 ddl=true")
	}
	if body["irreversible"] == true {
		t.Error("加列不该标不可逆")
	}
	pid, _ := body["id"].(string)
	if pid == "" {
		t.Fatal("预案缺少 id")
	}

	// 3. 应用
	code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply", map[string]any{}, true)
	if code != http.StatusOK {
		t.Fatalf("apply status = %d, body=%v", code, body)
	}
	undo, _ := body["undo"].(map[string]any)
	if undo == nil {
		t.Fatalf("加列应当可撤销, body=%v", body)
	}

	// 4. 结构里出现 note
	code, body = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/tables/st/schema", nil, true)
	if code != http.StatusOK {
		t.Fatalf("schema status = %d", code)
	}
	if !hasColumn(body, "note") {
		t.Fatalf("应用后结构里没有 note: %v", collectNames(body))
	}

	// 5. 撤销后 note 消失
	undoID, _ := undo["id"].(string)
	code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+undoID+"/apply", map[string]any{}, true)
	if code != http.StatusOK {
		t.Fatalf("undo status = %d, body=%v", code, body)
	}
	code, body = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/tables/st/schema", nil, true)
	if code != http.StatusOK {
		t.Fatalf("schema status = %d", code)
	}
	if hasColumn(body, "note") {
		t.Error("撤销后 note 仍在")
	}
}

// TestSchemaPlanRejectsInjection 验证结构参数的白名单：
// 类型与列名都是无法参数化的文本，必须挡住分号与引号。
func TestSchemaPlanRejectsInjection(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE st2 (id INTEGER PRIMARY KEY)"); code != http.StatusOK {
		t.Fatalf("建表 status = %d", code)
	}

	cases := []map[string]any{
		{"name": "a", "type": "TEXT; DROP TABLE st2"},
		{"name": "a\"b", "type": "TEXT"},
		{"name": "a", "type": "TEXT", "default": "nextval('s')"},
	}
	for _, col := range cases {
		code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/schema/plan", map[string]any{
			"table":  map[string]any{"kind": "table", "name": "st2"},
			"action": "add_column",
			"column": col,
		}, true)
		if code != http.StatusBadRequest {
			t.Fatalf("列定义 %v 应 400, 实际 %d, body=%v", col, code, body)
		}
		errObj, _ := body["error"].(map[string]any)
		if errObj["code"] != "INVALID_COLUMN" {
			t.Errorf("code = %v, 期望 INVALID_COLUMN", errObj["code"])
		}
	}

	// 表还在，说明注入没生效。
	if code, got := do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/tables/st2/schema", nil, true); code != http.StatusOK {
		t.Fatalf("st2 应仍存在, status = %d, body=%v", code, got)
	}
}

func hasColumn(body map[string]any, name string) bool {
	for _, c := range collectNames(body) {
		if c == name {
			return true
		}
	}
	return false
}

func collectNames(body map[string]any) []string {
	cols, _ := body["columns"].([]any)
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		m, _ := c.(map[string]any)
		if n, _ := m["name"].(string); n != "" {
			out = append(out, n)
		}
	}
	return out
}
