package api_test

import (
	"net/http"
	"strings"
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

// TestCancelWhenIdle 没有语句在跑时，取消返回 200 且 canceled=false。
//
// 这条守的是「取消不是错误」：界面上按晚了不该弹红字。
func TestCancelWhenIdle(t *testing.T) {
	_, ts, dir := newServer(t)
	connID := createConn(t, ts, dir, "test", "read-write")

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/cancel",
		map[string]any{}, true)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d %v", code, body)
	}
	if body["canceled"] != false {
		t.Errorf("空闲时 canceled 应为 false，实际 %v", body["canceled"])
	}
}

// TestExplainEndpoint 取执行计划：不执行语句本身，但能拿到计划行。
func TestExplainEndpoint(t *testing.T) {
	_, ts, dir := newServer(t)
	connID := createConn(t, ts, dir, "test", "read-write")
	execSQL(t, ts, connID, "CREATE TABLE items (id INTEGER PRIMARY KEY, v TEXT)")

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/explain",
		map[string]any{"sql": "SELECT * FROM items WHERE id = 1"}, true)
	if code != http.StatusOK {
		t.Fatalf("取计划失败: %d %v", code, body)
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("结果条数 = %d，响应：%v", len(results), body)
	}
	r0, _ := results[0].(map[string]any)
	sqlText, _ := r0["sql"].(string)
	if !strings.HasPrefix(strings.ToUpper(sqlText), "EXPLAIN QUERY PLAN") {
		t.Errorf("SQLite 上应该用 EXPLAIN QUERY PLAN，实际 %q", sqlText)
	}
	rows, _ := r0["rows"].([]any)
	if len(rows) == 0 {
		t.Error("执行计划不该是空结果")
	}
}

// TestExplainUsesCursor 多段脚本里只解释光标所在的那一条。
func TestExplainUsesCursor(t *testing.T) {
	_, ts, dir := newServer(t)
	connID := createConn(t, ts, dir, "test", "read-write")
	execSQL(t, ts, connID, "CREATE TABLE items (id INTEGER PRIMARY KEY, v TEXT)")

	first := "UPDATE items SET v = 'x' WHERE id = 1;"
	script := first + "\nSELECT * FROM items;"

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/explain",
		map[string]any{"sql": script, "cursor": len(first) + 2}, true)
	if code != http.StatusOK {
		t.Fatalf("取计划失败: %d %v", code, body)
	}
	results, _ := body["results"].([]any)
	r0, _ := results[0].(map[string]any)
	sqlText, _ := r0["sql"].(string)
	if strings.Contains(strings.ToUpper(sqlText), "UPDATE") {
		t.Errorf("解释的应该是 SELECT，实际 %q", sqlText)
	}
	if !strings.Contains(strings.ToUpper(sqlText), "SELECT * FROM ITEMS") {
		t.Errorf("解释的语句不对: %q", sqlText)
	}
}
