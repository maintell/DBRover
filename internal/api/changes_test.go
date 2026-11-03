package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestChangePlanUndoRestoresValue 验证「一键撤销」是一条完整闭环：
// 应用成功 → 拿到撤销预案 → 应用撤销 → 数据回到原值 → 撤销本身不能再被撤销。
func TestChangePlanUndoRestoresValue(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	if code := execApproved(t, ts, id, "CREATE TABLE u (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表 status = %d", code)
	}
	if code := execApproved(t, ts, id, "INSERT INTO u (id,v) VALUES (1,'a')"); code != http.StatusOK {
		t.Fatalf("插数据 status = %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/changes/plan", map[string]any{
		"table": map[string]any{"kind": "table", "name": "u"},
		"changes": []any{map[string]any{
			"op": "update", "key": map[string]any{"id": 1},
			"before": map[string]any{"id": 1, "v": "a"},
			"after":  map[string]any{"v": "a2"},
		}},
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("plan status = %d, body=%v", code, body)
	}
	pid, _ := body["id"].(string)

	code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply", map[string]any{}, true)
	if code != http.StatusOK {
		t.Fatalf("apply status = %d, body=%v", code, body)
	}
	undo, ok := body["undo"].(map[string]any)
	if !ok || undo == nil {
		t.Fatalf("应用成功后应返回撤销预案, body=%v", body)
	}
	undoID, _ := undo["id"].(string)
	if undoID == "" {
		t.Fatalf("撤销预案缺少 id: %v", undo)
	}
	if stmts, _ := undo["statements"].([]any); len(stmts) != 1 {
		t.Errorf("撤销语句数 = %d, 期望 1", len(stmts))
	}

	if got := valueOf(t, ts, id, "SELECT v FROM u WHERE id = 1"); got != "a2" {
		t.Fatalf("应用后 v = %v, 期望 a2", got)
	}

	// 撤销本身是普通预案，走同一套执行门。
	code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+undoID+"/apply", map[string]any{}, true)
	if code != http.StatusOK {
		t.Fatalf("undo apply status = %d, body=%v", code, body)
	}
	if body["undo"] != nil {
		t.Errorf("撤销不应再产生撤销预案, body=%v", body["undo"])
	}
	if got := valueOf(t, ts, id, "SELECT v FROM u WHERE id = 1"); got != "a" {
		t.Errorf("撤销后 v = %v, 期望 a", got)
	}

	code, _ = do(t, ts, http.MethodPost, "/api/v1/changes/"+undoID+"/apply", map[string]any{}, true)
	if code != http.StatusNotFound {
		t.Errorf("撤销重复应用 status = %d, 期望 404", code)
	}
}

// TestChangePlanUndoIsNullWhenIrreversible 验证「改不回来就说改不回来」：
// delete 没带整行原值时生成不出反向语句，接口明确返回 undo=null，
// 而不是给一个点了会失败的按钮。
func TestChangePlanUndoIsNullWhenIrreversible(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	if code := execApproved(t, ts, id, "CREATE TABLE u2 (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表 status = %d", code)
	}
	if code := execApproved(t, ts, id, "INSERT INTO u2 (id,v) VALUES (1,'a')"); code != http.StatusOK {
		t.Fatalf("插数据 status = %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/changes/plan", map[string]any{
		"table": map[string]any{"kind": "table", "name": "u2"},
		"changes": []any{map[string]any{
			"op": "delete", "key": map[string]any{"id": 1},
		}},
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("plan status = %d, body=%v", code, body)
	}
	if body["irreversible"] != true {
		t.Fatalf("缺整行原值的 delete 应标记不可逆, body=%v", body)
	}
	pid, _ := body["id"].(string)

	code, body = do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply", map[string]any{}, true)
	if code != http.StatusOK {
		t.Fatalf("apply status = %d, body=%v", code, body)
	}
	if v, ok := body["undo"]; !ok || v != nil {
		t.Errorf("不可逆变更的 undo 应为 null, 实际 %v", v)
	}
}

// valueOf 执行一条单值查询并返回结果。
func valueOf(t *testing.T, ts *httptest.Server, connID, sqlText string) any {
	t.Helper()
	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/exec", map[string]any{
		"sql": sqlText,
	}, true)
	if code != http.StatusOK {
		t.Fatalf("查询失败 status = %d, body=%v", code, body)
	}
	results, _ := body["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("查询无结果: %v", body)
	}
	rows, _ := results[0].(map[string]any)["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("期望 1 行, 实际 %d", len(rows))
	}
	return rows[0].([]any)[0]
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

// TestChangePlanRejectsBigIntegerAsJSONNumber 是验收项 6 的接口侧守卫：
// BIGINT 用 JSON 数字传（> 2^53）时必须 400，而不是把丢了精度的值写进去。
func TestChangePlanRejectsBigIntegerAsJSONNumber(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "test", "read-write")
	if code := execApproved(t, ts, id, "CREATE TABLE pint (id INTEGER PRIMARY KEY, big INTEGER)"); code != http.StatusOK {
		t.Fatalf("建表失败: %d", code)
	}
	if code := execApproved(t, ts, id, "INSERT INTO pint (id, big) VALUES (1, 1)"); code != http.StatusOK {
		t.Fatalf("造数失败: %d", code)
	}

	// 直接写原始 JSON：绕过 map[string]any 的 float64 转换，模拟真实客户端。
	body := `{"table":{"kind":"table","name":"pint"},` +
		`"changes":[{"op":"update","key":{"id":1},"before":{"id":1},` +
		`"after":{"big":9223372036854775807}}]}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/connections/"+id+"/changes/plan",
		strings.NewReader(body))
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

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, 期望 400, body=%v", resp.StatusCode, out)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != "NUMBER_PRECISION_LOSS" {
		t.Errorf("code = %v, 期望 NUMBER_PRECISION_LOSS", errObj["code"])
	}
	if errObj["hint"] == "" {
		t.Error("必须给出「改用字符串」的提示")
	}
}
