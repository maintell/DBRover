package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestSchemaPlanAcceptsCheckAndView 守住这两个动作在 HTTP 上真的可达。
//
// 为什么必须有这条：schemaPlanRequest 用 DisallowUnknownFields 解码，字段漏了
// 会得到「json: unknown field」——服务层测试直接调 BuildDDLPlan 全绿，界面点了
// 才发现根本没接到。这不是假设：add_check 与 create_view 第一次就是漏的。
func TestSchemaPlanAcceptsCheckAndView(t *testing.T) {
	_, ts, dir := newServer(t)
	connID := createConn(t, ts, dir, "test", "read-write")
	execSQL(t, ts, connID, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")

	// create_view：应当一路走到服务层并成功给出语句。
	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/schema/plan",
		map[string]any{
			"table":  map[string]any{"kind": "table", "name": "t"},
			"action": "create_view",
			"view":   map[string]any{"name": "v_api", "query": "SELECT id, v FROM t"},
		}, true)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create_view = %d %v", code, body)
	}
	if !strings.Contains(toJSON(body), "CREATE VIEW") {
		t.Errorf("预案里应有 CREATE VIEW：%v", body)
	}

	// add_check：SQLite 不支持，但错误必须是「引擎不支持」而不是「字段不认识」。
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/schema/plan",
		map[string]any{
			"table":  map[string]any{"kind": "table", "name": "t"},
			"action": "add_check",
			"check":  map[string]any{"name": "ck_v", "expression": "v IS NOT NULL"},
		}, true)
	if code != http.StatusBadRequest {
		t.Fatalf("add_check = %d %v", code, body)
	}
	if s := toJSON(body); strings.Contains(s, "unknown field") {
		t.Fatalf("请求字段没被识别：%s", s)
	}
	if s := toJSON(body); !strings.Contains(s, "CHECK") {
		t.Errorf("错误应说清是 CHECK 这条约束在 SQLite 上加不上：%s", s)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
