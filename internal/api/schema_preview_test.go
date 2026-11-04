package api_test

import (
	"net/http"
	"testing"
)

// TestSchemaPreviewDoesNotStorePlan 覆盖 docs/07 验收第 1 条的后端一半：
// 预览要能算语句，但**不能**在服务端留下可执行的预案——结构面板每敲一个字
// 都会调一次，若每次都存一张预案，服务端会被按键灌满。
func TestSchemaPreviewDoesNotStorePlan(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表失败: %d", code)
	}

	body := map[string]any{
		"table":  map[string]any{"kind": "table", "name": "t"},
		"action": "add_column",
		"column": map[string]any{"name": "note", "type": "TEXT", "nullable": true},
	}

	code, preview := do(t, ts, http.MethodPost,
		"/api/v1/connections/"+id+"/schema/plan?preview=1", body, true)
	if code != http.StatusOK {
		t.Fatalf("preview 状态码 = %d", code)
	}
	stmts, _ := preview["statements"].([]any)
	if len(stmts) == 0 {
		t.Fatalf("预览没有给出语句：%v", preview)
	}
	previewID, _ := preview["id"].(string)
	if previewID == "" {
		t.Fatal("预览应当带一个 id，便于前端去重")
	}

	// 拿着预览的 id 去执行：必须找不到——预览不保管预案。
	if code, _ := do(t, ts, http.MethodPost, "/api/v1/changes/"+previewID+"/apply", map[string]any{}, true); code != http.StatusNotFound {
		t.Fatalf("预览的预案不该能执行，实际状态码 = %d", code)
	}

	// 不带 preview 才会真的保管，并且能执行。
	code, planned := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/schema/plan", body, true)
	if code != http.StatusCreated {
		t.Fatalf("正式预案状态码 = %d，期望 201", code)
	}
	id2, _ := planned["id"].(string)
	if id2 == "" {
		t.Fatal("正式预案必须带 id")
	}
	if code, _ := do(t, ts, http.MethodPost, "/api/v1/changes/"+id2+"/apply", map[string]any{}, true); code != http.StatusOK {
		t.Fatalf("正式预案应当能执行，实际状态码 = %d", code)
	}
}
