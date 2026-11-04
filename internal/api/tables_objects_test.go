package api_test

import (
	"net/http"
	"testing"
)

// TestTablesIncludeTriggers 锁住「能力声明 → 对象树」这条链。
//
// 触发器能被 Metadata 列出来是一回事，能不能出现在 /tables 与界面上是另一回事；
// 少了这一步，能力位说 true 而用户永远看不到。
func TestTablesIncludeTriggers(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	for _, q := range []string{
		"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)",
		"CREATE TABLE audit (note TEXT)",
		"CREATE TRIGGER trg_t AFTER INSERT ON t BEGIN INSERT INTO audit (note) VALUES ('x'); END",
	} {
		if code := execApproved(t, ts, id, q); code != http.StatusOK {
			t.Fatalf("准备数据失败 (%s): %d", q, code)
		}
	}

	code, body := do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/tables", nil, true)
	if code != http.StatusOK {
		t.Fatalf("tables 状态码 = %d", code)
	}
	items, _ := body["items"].([]any)
	found := false
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["name"] != "trg_t" {
			continue
		}
		found = true
		if m["kind"] != "trigger" {
			t.Errorf("trg_t 的 kind = %v，期望 trigger", m["kind"])
		}
	}
	if !found {
		t.Fatalf("触发器没出现在对象列表里：%v", items)
	}
}
