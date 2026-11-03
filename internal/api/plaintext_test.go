package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/api"
	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// secretForTest 是一个足够特别、不会误命中别处内容的字符串。
const secretForTest = "S3cr3t-Passw0rd-9f2a"

// getRawBody 取原始响应体：这个用例关心的是「字节里有没有那个口令」，
// 解析成 map 再判断会漏掉未声明的字段。
func getRawBody(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("X-DBRover-Token", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应: %v", err)
	}
	return string(b)
}

// TestNoPlaintextPassword 对应 docs/10 验收 7：
// 配置序列化、日志、审计三处均无明文密码。
func TestNoPlaintextPassword(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	log, err := service.NewAuditLog(auditPath, 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	opener := func(engine model.Engine, cfg driver.ConnectConfig) (any, error) {
		c := sqlite.New()
		if err := c.Open(context.Background(), cfg); err != nil {
			return nil, err
		}
		return c, nil
	}
	s := api.New(testToken, opener, log)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		s.Close()
	})

	// 建连接时带上口令。SQLite 用不到它，但它会经过整条链路——
	// 这正是要验证的：用不到也不许回显。
	code, body := do(t, ts, http.MethodPost, "/api/v1/connections", map[string]any{
		"name": "plain", "engine": "sqlite",
		"host":        filepath.Join(dir, "plain.db"),
		"password":    secretForTest,
		"environment": "test", "access_mode": "read-write",
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("建连接失败: %d %v", code, body)
	}

	// 1) 连接列表（配置序列化）
	raw := getRawBody(t, ts, "/api/v1/connections")
	if strings.Contains(raw, secretForTest) {
		t.Errorf("连接列表里出现了明文口令: %s", raw)
	}

	// 2) 审计接口 + 审计文件（执行一次产生事件）
	if code, b := do(t, ts, http.MethodPost, "/api/v1/connections/conn-1/exec",
		map[string]any{"sql": "SELECT 1"}, true); code != http.StatusOK {
		t.Fatalf("执行失败: %d %v", code, b)
	}
	if raw := getRawBody(t, ts, "/api/v1/audit"); strings.Contains(raw, secretForTest) {
		t.Errorf("审计接口里出现了明文口令: %s", raw)
	}
	file, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("读审计文件: %v", err)
	}
	if strings.Contains(string(file), secretForTest) {
		t.Errorf("审计文件里出现了明文口令: %s", string(file))
	}

	// 3) 改口令类语句：即使语句本身执行失败（SQLite 没有 ALTER USER），
	// 审计里记的文本也必须已经脱敏——这条走的是真实的 record 路径。
	do(t, ts, http.MethodPost, "/api/v1/connections/conn-1/exec", map[string]any{
		"sql": "ALTER USER 'app' IDENTIFIED BY '" + secretForTest + "'",
	}, true)
	file, err = os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("读审计文件: %v", err)
	}
	if strings.Contains(string(file), secretForTest) {
		t.Errorf("改口令语句的口令进了审计文件: %s", string(file))
	}
	if !strings.Contains(string(file), "IDENTIFIED BY '***'") {
		t.Errorf("脱敏后应保留上下文（IDENTIFIED BY + ***）: %s", string(file))
	}
	// 审计接口同样不能带出来。
	if raw := getRawBody(t, ts, "/api/v1/audit"); strings.Contains(raw, secretForTest) {
		t.Errorf("审计接口里出现了改口令语句的口令: %s", raw)
	}
}
