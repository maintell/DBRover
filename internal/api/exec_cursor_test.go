package api_test

import (
	"net/http"
	"testing"
)

// TestExecWithCursorRunsOneStatement 对应 docs/05 验收 1：
// 给了光标偏移就只执行那一条，前后两条都不生效。
//
// 切分与挑选都在服务端：前端只提供偏移，不重复实现方言规则。
func TestExecWithCursorRunsOneStatement(t *testing.T) {
	_, ts, dir := newServer(t)
	connID := createConn(t, ts, dir, "test", "read-write")
	execSQL(t, ts, connID, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)")
	execSQL(t, ts, connID, "INSERT INTO t (id, v) VALUES (1, 'a')")

	first := "UPDATE t SET v = 'first' WHERE id = 1;"
	script := first + "\nSELECT v FROM t;\nUPDATE t SET v = 'third' WHERE id = 1;"
	cursor := len(first) + 2 // 光标落在第二句里

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/exec",
		map[string]any{"sql": script, "cursor": cursor}, true)
	if code == http.StatusPreconditionRequired {
		er, _ := body["error"].(map[string]any)
		ap, _ := er["approval"].(map[string]any)
		token, _ := ap["token"].(string)
		code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/exec",
			map[string]any{"sql": script, "cursor": cursor, "approval_token": token}, true)
	}
	if code != http.StatusOK {
		t.Fatalf("执行失败: %d %v", code, body)
	}

	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("只应执行一条，实际 %d 条: %v", len(results), results)
	}
	r0, _ := results[0].(map[string]any)
	if r0["sql"] != "SELECT v FROM t" {
		t.Errorf("执行的是 %q", r0["sql"])
	}

	// 前后的 UPDATE 都不能生效。
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/exec",
		map[string]any{"sql": "SELECT v FROM t WHERE id = 1"}, true)
	if code != http.StatusOK {
		t.Fatalf("复核查询失败: %d %v", code, body)
	}
	rows := rowsOf(t, body)
	if len(rows) != 1 || rows[0][0] != "a" {
		t.Fatalf("数据被改了: %v", rows)
	}
}

// rowsOf 从 /exec 的响应里取出第一个结果集的所有行。
func rowsOf(t *testing.T, body map[string]any) [][]any {
	t.Helper()
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatal("响应里没有 results")
	}
	r0, _ := results[0].(map[string]any)
	raw, _ := r0["rows"].([]any)
	out := make([][]any, 0, len(raw))
	for _, row := range raw {
		cells, _ := row.([]any)
		out = append(out, cells)
	}
	return out
}
