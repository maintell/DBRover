package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doRaw 发一个请求并把响应体原样读回来，用于校验 NDJSON 流。
func doRaw(t *testing.T, ts *httptest.Server, method, path string, body any) (int, string, string) {
	t.Helper()
	var buf *bytes.Reader
	if body == nil {
		buf = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-DBRover-Token", testToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(raw)
}

// TestBulkPlanLifecycle 走一遍条件批量修改：出预案 → 应用 → 值变了。
func TestBulkPlanLifecycle(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE blk (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表失败: %d", code)
	}
	if code := execApproved(t, ts, id, "INSERT INTO blk (id,v) VALUES (1,'a'),(2,'a'),(3,'b')"); code != http.StatusOK {
		t.Fatalf("插数据失败: %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/bulk/plan", map[string]any{
		"table":  map[string]any{"kind": "table", "name": "blk"},
		"set":    map[string]any{"v": "done"},
		"filter": []any{map[string]any{"column": "v", "op": "eq", "value": "a"}},
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("bulk/plan status = %d, body=%v", code, body)
	}
	if body["affected"] != float64(2) {
		t.Errorf("affected = %v, 期望 2", body["affected"])
	}
	samples, _ := body["samples"].([]any)
	if len(samples) != 2 {
		t.Errorf("样本数 = %d, 期望 2", len(samples))
	}
	plan, _ := body["plan"].(map[string]any)
	pid, _ := plan["id"].(string)
	if pid == "" {
		t.Fatal("预案缺少 id")
	}

	if code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply", map[string]any{}, true); code != http.StatusOK {
		t.Fatalf("apply status = %d, body=%v", code, body)
	}

	// 只有 v='a' 的两行被改，v='b' 的那行不变。
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "SELECT COUNT(*) AS n FROM blk WHERE v = 'done'",
	}, true)
	if code != http.StatusOK {
		t.Fatalf("查询失败: %d", code)
	}
	results, _ := body["results"].([]any)
	rows, _ := results[0].(map[string]any)["rows"].([]any)
	if len(rows) != 1 || rows[0].([]any)[0] != float64(2) {
		t.Errorf("被改行数不对: %v", rows)
	}
}

// TestBulkPlanRequiresTableName 验证无条件批量修改必须回填表名（428）。
func TestBulkPlanRequiresTableName(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE blk2 (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表失败: %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/bulk/plan", map[string]any{
		"table": map[string]any{"kind": "table", "name": "blk2"},
		"set":   map[string]any{"v": "all"},
	}, true)
	if code != http.StatusPreconditionRequired {
		t.Fatalf("无条件批量修改应 428, 实际 %d, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "BULK_CONFIRM_TABLE_REQUIRED" {
		t.Errorf("code = %v", errObj["code"])
	}

	// 回填表名后放行。
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/bulk/plan", map[string]any{
		"table":         map[string]any{"kind": "table", "name": "blk2"},
		"set":           map[string]any{"v": "all"},
		"confirm_table": "blk2",
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("回填表名后应 201, 实际 %d, body=%v", code, body)
	}
}

// approvalTokenOf 从 428 响应里取出审批凭证。
func approvalTokenOf(t *testing.T, raw string) string {
	t.Helper()
	var refused struct {
		Error struct {
			Approval struct {
				Token string `json:"token"`
			} `json:"approval"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &refused); err != nil {
		t.Fatalf("428 body 解析失败: %v (%s)", err, raw)
	}
	if refused.Error.Approval.Token == "" {
		t.Fatalf("428 未带审批凭证: %s", raw)
	}
	return refused.Error.Approval.Token
}

// TestBulkApplyStreamsBatchProgress 对应 docs/06 §3.2 与验收项 8：
// 影响行数超过单批上限时，apply 用 NDJSON 逐批回报进度，最后一条是结果。
//
// 断言的重点不是「有没有进度」，而是「批次数与已提交行数对得上」：
// 只有真的分批推进才可能产生 3 条进度。
func TestBulkApplyStreamsBatchProgress(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "test", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE big (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表失败: %d", code)
	}
	if code := execApproved(t, ts, id, `
		INSERT INTO big (id, v)
		WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM seq WHERE x < 2500)
		SELECT x, 'x' FROM seq`); code != http.StatusOK {
		t.Fatalf("造数失败: %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/bulk/plan", map[string]any{
		"table":              map[string]any{"kind": "table", "name": "big"},
		"set":                map[string]any{"v": "done"},
		"confirm_table":      "big",
		"accept_no_rollback": true,
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("bulk/plan status = %d, body=%v", code, body)
	}
	if body["batched"] != true {
		t.Fatalf("2500 行应标记为分批执行, body=%v", body)
	}
	if body["batches"] != float64(3) {
		t.Errorf("batches = %v, 期望 3", body["batches"])
	}
	plan, _ := body["plan"].(map[string]any)
	pid, _ := plan["id"].(string)
	if pid == "" {
		t.Fatal("预案缺少 id")
	}

	status, ctype, raw := doRaw(t, ts, http.MethodPost,
		"/api/v1/changes/"+pid+"/apply?stream=1", map[string]any{})
	// 2500 行的全表写是 R4/F3，第一次必须先被拦下——而且拦下时还没写任何进度，
	// 所以这里仍然是普通 JSON 的 428，不是一条流。
	if status != http.StatusPreconditionRequired {
		t.Fatalf("第一次 apply 应 428, 实际 %d, raw=%s", status, raw)
	}
	if !strings.Contains(ctype, "application/json") {
		t.Fatalf("被拦下时 Content-Type = %q, 期望 JSON", ctype)
	}
	var refused struct {
		Error struct {
			Approval struct {
				Token string `json:"token"`
			} `json:"approval"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &refused); err != nil {
		t.Fatalf("428 body 解析失败: %v (%s)", err, raw)
	}
	apprToken := refused.Error.Approval.Token
	if apprToken == "" {
		t.Fatal("428 未带审批凭证")
	}

	status, ctype, raw = doRaw(t, ts, http.MethodPost,
		"/api/v1/changes/"+pid+"/apply?stream=1", map[string]any{"approval_token": apprToken})
	if status != http.StatusOK {
		t.Fatalf("流式 apply status = %d, raw=%s", status, raw)
	}
	if !strings.Contains(ctype, "application/x-ndjson") {
		t.Fatalf("Content-Type = %q, 期望 ndjson", ctype)
	}

	var progress []float64
	var result map[string]any
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	for _, ln := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("NDJSON 行不是合法 JSON: %q (%v)", ln, err)
		}
		switch ev["type"] {
		case "progress":
			progress = append(progress, ev["done"].(float64))
		case "result":
			result, _ = ev["body"].(map[string]any)
			if ev["status"] != float64(http.StatusOK) {
				t.Errorf("result 行状态 = %v", ev["status"])
			}
		case "error":
			t.Fatalf("不该出现 error 行: %v", ev)
		}
	}
	if len(progress) != 3 {
		t.Fatalf("进度行数 = %d (%v), 期望 3", len(progress), progress)
	}
	for i, want := range []float64{1000, 2000, 2500} {
		if progress[i] != want {
			t.Errorf("第 %d 条进度 = %v, 期望 %v", i+1, progress[i], want)
		}
	}
	if result == nil {
		t.Fatal("缺少 result 行")
	}
	if result["rows_affected"] != float64(2500) {
		t.Errorf("rows_affected = %v, 期望 2500", result["rows_affected"])
	}
	if result["undo"] != nil {
		t.Errorf("超过回滚上限时 undo 应为 null, 实际 %v", result["undo"])
	}

	// 数据确实全改了。
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "SELECT COUNT(*) AS n FROM big WHERE v = 'done'",
	}, true)
	if code != http.StatusOK {
		t.Fatalf("查询失败: %d", code)
	}
	results, _ := body["results"].([]any)
	rows, _ := results[0].(map[string]any)["rows"].([]any)
	if len(rows) != 1 || rows[0].([]any)[0] != float64(2500) {
		t.Errorf("被改行数不对: %v", rows)
	}
}

// TestBulkApplyWithoutStreamKeepsJSON 验证不带 stream 时仍是普通 JSON：
// 进度流是可选形态，不能让既有调用方拿到一个读不懂的 body。
func TestBulkApplyWithoutStreamKeepsJSON(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "test", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE big2 (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表失败: %d", code)
	}
	if code := execApproved(t, ts, id, "INSERT INTO big2 (id, v) VALUES (1,'x'),(2,'x')"); code != http.StatusOK {
		t.Fatalf("造数失败: %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/bulk/plan", map[string]any{
		"table":         map[string]any{"kind": "table", "name": "big2"},
		"set":           map[string]any{"v": "done"},
		"confirm_table": "big2",
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("bulk/plan status = %d, body=%v", code, body)
	}
	if body["batched"] != false {
		t.Errorf("少量行不该分批, body=%v", body["batched"])
	}
	plan, _ := body["plan"].(map[string]any)
	pid, _ := plan["id"].(string)

	status, ctype, raw := doRaw(t, ts, http.MethodPost,
		"/api/v1/changes/"+pid+"/apply", map[string]any{})
	if status != http.StatusPreconditionRequired {
		t.Fatalf("全表写应 428, 实际 %d, raw=%s", status, raw)
	}
	status, ctype, raw = doRaw(t, ts, http.MethodPost,
		"/api/v1/changes/"+pid+"/apply", map[string]any{"approval_token": approvalTokenOf(t, raw)})
	if status != http.StatusOK {
		t.Fatalf("apply status = %d, raw=%s", status, raw)
	}
	if !strings.Contains(ctype, "application/json") {
		t.Fatalf("Content-Type = %q, 期望 JSON", ctype)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("不是单个 JSON 对象: %v (%s)", err, raw)
	}
	if out["rows_affected"] != float64(2) {
		t.Errorf("rows_affected = %v, 期望 2", out["rows_affected"])
	}
}

// TestBulkApplyStreamOnUnbatchedPlanStaysJSON 验证「请求了进度流但没有进度可报」
// 仍然得到一份普通 JSON：状态码与错误体不能被流形态破坏，否则前端要先猜格式。
func TestBulkApplyStreamOnUnbatchedPlanStaysJSON(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "test", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE big3 (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表失败: %d", code)
	}
	if code := execApproved(t, ts, id, "INSERT INTO big3 (id, v) VALUES (1,'x'),(2,'x')"); code != http.StatusOK {
		t.Fatalf("造数失败: %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/bulk/plan", map[string]any{
		"table":         map[string]any{"kind": "table", "name": "big3"},
		"set":           map[string]any{"v": "done"},
		"confirm_table": "big3",
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("bulk/plan status = %d, body=%v", code, body)
	}
	plan, _ := body["plan"].(map[string]any)
	pid, _ := plan["id"].(string)

	status, _, raw := doRaw(t, ts, http.MethodPost,
		"/api/v1/changes/"+pid+"/apply?stream=1", map[string]any{})
	if status != http.StatusPreconditionRequired {
		t.Fatalf("未分批时即使要了流也应照常返回 428, 实际 %d, raw=%s", status, raw)
	}
	status, ctype, raw := doRaw(t, ts, http.MethodPost,
		"/api/v1/changes/"+pid+"/apply?stream=1", map[string]any{"approval_token": approvalTokenOf(t, raw)})
	if status != http.StatusOK {
		t.Fatalf("apply status = %d, raw=%s", status, raw)
	}
	if !strings.Contains(ctype, "application/json") {
		t.Fatalf("没有进度可报时应退回普通 JSON, Content-Type = %q", ctype)
	}
	if strings.Contains(raw, `"type"`) {
		t.Errorf("不该出现 NDJSON 行: %s", raw)
	}
}
