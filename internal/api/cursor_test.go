package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func seedRows(t *testing.T, ts *httptest.Server, connID string, n int) {
	t.Helper()
	if code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/exec", map[string]any{
		"sql": "CREATE TABLE s (id INTEGER PRIMARY KEY)",
	}, true); code != http.StatusOK {
		t.Fatalf("建表失败: %d %v", code, body)
	}
	vals := make([]string, n)
	for i := 1; i <= n; i++ {
		vals[i-1] = fmt.Sprintf("(%d)", i)
	}
	if code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/exec", map[string]any{
		"sql": "INSERT INTO s (id) VALUES " + strings.Join(vals, ","),
	}, true); code != http.StatusOK {
		t.Fatalf("插数据失败: %d %v", code, body)
	}
}

func TestStreamingQueryPagination(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	seedRows(t, ts, id, 25)

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/query", map[string]any{
		"sql":        "SELECT id FROM s ORDER BY id",
		"fetch_size": 5,
	}, true)
	if code != http.StatusOK {
		t.Fatalf("query status = %d, body=%v", code, body)
	}
	cid, _ := body["cursor_id"].(string)
	if cid == "" {
		t.Fatal("响应缺少 cursor_id")
	}
	if body["risk"] != "R0" {
		t.Errorf("risk = %v, 期望 R0", body["risk"])
	}
	cols, _ := body["columns"].([]any)
	if len(cols) != 1 {
		t.Fatalf("列数 = %d", len(cols))
	}

	total := 0
	pages := 0
	for {
		code, body = do(t, ts, http.MethodGet, "/api/v1/results/"+cid+"/page?limit=10", nil, true)
		if code != http.StatusOK {
			t.Fatalf("page status = %d, body=%v", code, body)
		}
		rows, _ := body["rows"].([]any)
		total += len(rows)
		pages++
		if body["has_more"] != true {
			break
		}
		if pages > 10 {
			t.Fatal("分页未收敛")
		}
	}
	if total != 25 {
		t.Errorf("共取到 %d 行, 期望 25", total)
	}
	if pages != 3 {
		t.Errorf("页数 = %d, 期望 3", pages)
	}

	// 读尽后游标应已关闭
	code, body = do(t, ts, http.MethodGet, "/api/v1/results/"+cid+"/page", nil, true)
	if code != http.StatusGone {
		t.Fatalf("读尽后 status = %d, 期望 410, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "CURSOR_CLOSED" {
		t.Errorf("code = %v", errObj["code"])
	}
}

func TestQueryEndpointRejectsWrite(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	seedRows(t, ts, id, 3)

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/query", map[string]any{
		"sql": "DELETE FROM s",
	}, true)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, 期望 400, body=%v", code, body)
	}
}

func TestCloseCursorEndpoint(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	seedRows(t, ts, id, 5)

	_, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/query", map[string]any{
		"sql": "SELECT id FROM s",
	}, true)
	cid, _ := body["cursor_id"].(string)

	code, _ := do(t, ts, http.MethodDelete, "/api/v1/results/"+cid, nil, true)
	if code != http.StatusNoContent {
		t.Fatalf("关闭游标 status = %d, 期望 204", code)
	}
	code, _ = do(t, ts, http.MethodGet, "/api/v1/results/"+cid+"/page", nil, true)
	if code != http.StatusGone {
		t.Fatalf("关闭后 status = %d, 期望 410", code)
	}
}

func TestTableRowsEndpointKeyset(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	seedRows(t, ts, id, 25)

	base := "/api/v1/connections/" + id + "/tables/s/rows"
	after := ""
	seen := map[float64]bool{}
	pages := 0

	for {
		u := base + "?limit=10"
		if after != "" {
			u += "&after=" + url.QueryEscape(after)
		}
		code, body := do(t, ts, http.MethodGet, u, nil, true)
		if code != http.StatusOK {
			t.Fatalf("rows status = %d, body=%v", code, body)
		}
		rows, _ := body["rows"].([]any)
		pages++
		for _, r := range rows {
			row := r.([]any)
			v := row[0].(float64)
			if seen[v] {
				t.Fatalf("键集分页出现重复行: %v", v)
			}
			seen[v] = true
		}
		if body["has_more"] != true {
			break
		}
		next, _ := body["next"].([]any)
		if len(next) != 1 {
			t.Fatalf("next 长度 = %d, 期望 1", len(next))
		}
		after = fmt.Sprintf(`["%v"]`, next[0])
		if pages > 10 {
			t.Fatal("分页未收敛")
		}
	}

	if len(seen) != 25 {
		t.Errorf("共取到 %d 行, 期望 25", len(seen))
	}
	if pages != 3 {
		t.Errorf("页数 = %d, 期望 3", pages)
	}
}

func TestTableRowsEndpointNoPrimaryKey(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	if code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "CREATE TABLE nopk (a TEXT)",
	}, true); code != http.StatusOK {
		t.Fatalf("建表失败: %d %v", code, body)
	}

	code, body := do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/tables/nopk/rows", nil, true)
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, 期望 501, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "CAPABILITY_UNSUPPORTED" {
		t.Errorf("code = %v", errObj["code"])
	}
}
