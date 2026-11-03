package api_test

import (
	"net/http"
	"testing"
)

// TestSchemaCreateTableAndUndo 走一遍建表闭环：
// 生成预案 → 应用 → 表出现 → 撤销（DROP TABLE）→ 表消失。
func TestSchemaCreateTableAndUndo(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/schema/plan", map[string]any{
		"table":  map[string]any{"kind": "table", "name": "created"},
		"action": "create_table",
		"columns": []any{
			map[string]any{"name": "id", "type": "INTEGER"},
			map[string]any{"name": "label", "type": "TEXT", "nullable": true},
		},
		"primary_key": []string{"id"},
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("create_table plan status = %d, body=%v", code, body)
	}
	if body["ddl"] != true || body["irreversible"] == true {
		t.Errorf("建表预案标记不对: %v", body)
	}
	pid, _ := body["id"].(string)

	code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply", map[string]any{}, true)
	if code != http.StatusOK {
		t.Fatalf("apply status = %d, body=%v", code, body)
	}
	undo, _ := body["undo"].(map[string]any)
	if undo == nil {
		t.Fatalf("建表应当可撤销, body=%v", body)
	}

	// 表已存在，且能看到两列。
	code, body = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/tables/created/schema", nil, true)
	if code != http.StatusOK {
		t.Fatalf("新建表的 schema status = %d, body=%v", code, body)
	}
	if !hasColumn(body, "id") || !hasColumn(body, "label") {
		t.Fatalf("新表列不对: %v", collectNames(body))
	}

	// 撤销 = DROP TABLE。用表清单判定存在性：
	// 对已删除的表 DescribeTable 的失败行为因引擎而异，清单才是准的。
	undoID, _ := undo["id"].(string)
	code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+undoID+"/apply", map[string]any{}, true)
	if code != http.StatusOK {
		t.Fatalf("undo status = %d, body=%v", code, body)
	}
	code, body = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/tables", nil, true)
	if code != http.StatusOK {
		t.Fatalf("tables status = %d", code)
	}
	for _, it := range asItems(body) {
		if it == "created" {
			t.Fatal("撤销后表仍在清单里")
		}
	}
}

// asItems 取出表清单里的名字。
func asItems(body map[string]any) []string {
	raw, _ := body["items"].([]any)
	out := make([]string, 0, len(raw))
	for _, it := range raw {
		m, _ := it.(map[string]any)
		if n, _ := m["name"].(string); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// TestSchemaCreateTableRejectsWithoutPrimaryKey 验证「建表必须给主键」这条 v1 约束
// 落到接口上是 400 而不是建出一张不可编辑的表。
func TestSchemaCreateTableRejectsWithoutPrimaryKey(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/schema/plan", map[string]any{
		"table":   map[string]any{"kind": "table", "name": "nopk_new"},
		"action":  "create_table",
		"columns": []any{map[string]any{"name": "a", "type": "TEXT"}},
	}, true)
	if code != http.StatusBadRequest {
		t.Fatalf("无主键建表应 400, 实际 %d, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "INVALID_COLUMN" {
		t.Errorf("code = %v, 期望 INVALID_COLUMN", errObj["code"])
	}
}
