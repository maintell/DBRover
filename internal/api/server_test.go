package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/maintell/DBRover/internal/api"
	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

const testToken = "test-token-123"

func newServer(t *testing.T) (*api.Server, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	opener := func(engine model.Engine, cfg driver.ConnectConfig) (driver.Connector, error) {
		c := sqlite.New()
		if err := c.Open(context.Background(), cfg); err != nil {
			return nil, err
		}
		return c, nil
	}
	s := api.New(testToken, opener, &service.SliceAudit{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		s.Close()
	})
	return s, ts, dir
}

func do(t *testing.T, ts *httptest.Server, method, path string, body any, withToken bool) (int, map[string]any) {
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
	if withToken {
		req.Header.Set("X-DBRover-Token", testToken)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func createConn(t *testing.T, ts *httptest.Server, dir, env, mode string) string {
	t.Helper()
	code, body := do(t, ts, http.MethodPost, "/api/v1/connections", map[string]any{
		"name":        "test",
		"engine":      "sqlite",
		"host":        filepath.Join(dir, "api.db"),
		"environment": env,
		"access_mode": mode,
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("创建连接失败: %d %v", code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatal("响应缺少 id")
	}
	return id
}

func TestPingNeedsNoToken(t *testing.T) {
	_, ts, _ := newServer(t)
	code, body := do(t, ts, http.MethodGet, "/api/v1/health", nil, false)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%v", code, body)
	}
}

func TestAuthRequired(t *testing.T) {
	_, ts, _ := newServer(t)
	code, body := do(t, ts, http.MethodGet, "/api/v1/connections", nil, false)
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, 期望 401, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "AUTH_TOKEN_INVALID" {
		t.Errorf("code = %v", errObj["code"])
	}

	code, _ = do(t, ts, http.MethodGet, "/api/v1/connections", nil, true)
	if code != http.StatusOK {
		t.Fatalf("带 token 时 status = %d", code)
	}
}

func TestConnectionLifecycle(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "test", "read-write")

	code, body := do(t, ts, http.MethodGet, "/api/v1/connections", nil, true)
	if code != http.StatusOK {
		t.Fatalf("list status = %d", code)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("连接数 = %d, 期望 1", len(items))
	}

	code, body = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/health", nil, true)
	if code != http.StatusOK {
		t.Fatalf("health status = %d, body=%v", code, body)
	}
	if body["read_only"] != false {
		t.Errorf("read_only = %v, 期望 false", body["read_only"])
	}

	code, _ = do(t, ts, http.MethodDelete, "/api/v1/connections/"+id, nil, true)
	if code != http.StatusNoContent {
		t.Fatalf("delete status = %d", code)
	}
	code, _ = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/health", nil, true)
	if code != http.StatusNotFound {
		t.Fatalf("删除后 status = %d, 期望 404", code)
	}
}

func TestExecQueryAndTables(t *testing.T) {
	_, ts, dir := newServer(t)
	// local 环境：DDL 为 F1（内联预览），不需要额外审批。
	id := createConn(t, ts, dir, "local", "read-write")

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)",
	}, true)
	if code != http.StatusOK {
		t.Fatalf("建表 status = %d, body=%v", code, body)
	}

	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "INSERT INTO t (id, v) VALUES (1, 'a')",
	}, true)
	if code != http.StatusOK {
		t.Fatalf("插入 status = %d, body=%v", code, body)
	}

	code, body = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/tables", nil, true)
	if code != http.StatusOK {
		t.Fatalf("tables status = %d", code)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("表数 = %d, 期望 1", len(items))
	}

	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "SELECT id, v FROM t",
	}, true)
	if code != http.StatusOK {
		t.Fatalf("查询 status = %d, body=%v", code, body)
	}
	if body["risk"] != "R0" {
		t.Errorf("risk = %v, 期望 R0", body["risk"])
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("结果数 = %d", len(results))
	}
}

func TestExecNeedsApprovalInProduction(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "production", "read-write")

	if code := execApproved(t, ts, id, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表 status = %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "UPDATE t SET v='x' WHERE id > 0",
	}, true)
	if code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, 期望 428, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "POLICY_APPROVAL_REQUIRED" {
		t.Fatalf("code = %v", errObj["code"])
	}

	appr, _ := errObj["approval"].(map[string]any)
	token, _ := appr["token"].(string)
	if token == "" {
		t.Fatalf("428 响应必须带审批凭证: %v", errObj)
	}
	if appr["statement_hash"] == "" || appr["expires_at"] == "" {
		t.Errorf("审批凭证必须绑定语句指纹与有效期: %v", appr)
	}

	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql":            "UPDATE t SET v='x' WHERE id > 0",
		"approval_token": token,
	}, true)
	if code != http.StatusOK {
		t.Fatalf("带凭证后 status = %d, body=%v", code, body)
	}
}

// execApproved 先执行一次拿审批凭证，再带凭证重试，返回最终状态码。
func execApproved(t *testing.T, ts *httptest.Server, connID, sqlText string) int {
	t.Helper()
	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/exec", map[string]any{
		"sql": sqlText,
	}, true)
	if code != http.StatusPreconditionRequired {
		return code
	}
	errObj, _ := body["error"].(map[string]any)
	appr, _ := errObj["approval"].(map[string]any)
	token, _ := appr["token"].(string)
	if token == "" {
		t.Fatalf("428 未带审批凭证: %v", errObj)
	}
	code, _ = do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/exec", map[string]any{
		"sql":            sqlText,
		"approval_token": token,
	}, true)
	return code
}

// TestApprovalTokenBindsToStatement 是 ADR-0003 的核心回归：
// 用 A 语句换来的凭证去执行 B 语句，必须被拒。
func TestApprovalTokenBindsToStatement(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "production", "read-write")

	if code := execApproved(t, ts, id, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("建表 status = %d", code)
	}

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "UPDATE t SET v='a' WHERE id > 0",
	}, true)
	if code != http.StatusPreconditionRequired {
		t.Fatalf("首次 status = %d, 期望 428, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	appr, _ := errObj["approval"].(map[string]any)
	token, _ := appr["token"].(string)

	// 换一条语句复用同一凭证
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql":            "DELETE FROM t WHERE id > 0",
		"approval_token": token,
	}, true)
	if code != http.StatusConflict {
		t.Fatalf("错配 status = %d, 期望 409, body=%v", code, body)
	}
	errObj, _ = body["error"].(map[string]any)
	if errObj["code"] != "POLICY_APPROVAL_MISMATCH" {
		t.Errorf("code = %v, 期望 POLICY_APPROVAL_MISMATCH", errObj["code"])
	}
}

// TestApprovalTokenRejectsForgery 保证伪造凭证无效。
func TestApprovalTokenRejectsForgery(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "production", "read-write")

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql":            "DELETE FROM t",
		"approval_token": "9999999999.deadbeef",
	}, true)
	if code != http.StatusConflict {
		t.Fatalf("伪造凭证 status = %d, 期望 409, body=%v", code, body)
	}
}

func TestExecReadOnlyRejected(t *testing.T) {
	_, ts, dir := newServer(t)

	// 先建库并落一张表，否则只读打开会因文件不存在而失败。
	rw := createConn(t, ts, dir, "test", "read-write")
	do(t, ts, http.MethodPost, "/api/v1/connections/"+rw+"/exec", map[string]any{
		"sql":     "CREATE TABLE t (id INTEGER PRIMARY KEY)",
		"approve": true,
	}, true)
	do(t, ts, http.MethodDelete, "/api/v1/connections/"+rw, nil, true)

	ro := createConn(t, ts, dir, "test", "read-only")
	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+ro+"/exec", map[string]any{
		"sql": "DELETE FROM t",
	}, true)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, 期望 403, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "POLICY_READ_ONLY" {
		t.Errorf("code = %v", errObj["code"])
	}
}

func TestAuditEndpoint(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "test", "read-write")
	do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{"sql": "SELECT 1"}, true)

	code, body := do(t, ts, http.MethodGet, "/api/v1/audit", nil, true)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("审计条数 = %d, 期望 1", len(items))
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "test", "read-write")
	code, _ := do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/exec", map[string]any{
		"sql": "SELECT 1", "bogus": true,
	}, true)
	if code != http.StatusBadRequest {
		t.Fatalf("未知字段应返回 400, 实际 %d", code)
	}
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"localhost:8080": true,
		"[::1]:8080":     true,
		"0.0.0.0:8080":   false,
		"10.0.0.1:8080":  false,
		"bad":            false,
	}
	for addr, want := range cases {
		if got := api.IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, 期望 %v", addr, got, want)
		}
	}
}

func TestServeRejectsNonLoopback(t *testing.T) {
	s := api.New(testToken, nil, nil)
	err := s.Serve(context.Background(), "0.0.0.0:9999", false)
	if err == nil {
		t.Fatal("非回环地址应被拒绝")
	}
}
