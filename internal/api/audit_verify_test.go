package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/maintell/DBRover/internal/api"
	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
	"github.com/maintell/DBRover/internal/service"
)

// TestAuditVerifyUnsupportedByMemorySink 内存实现没有链条可校验，必须明说，
// 不能返回一个看起来通过的 ok=true。
func TestAuditVerifyUnsupportedByMemorySink(t *testing.T) {
	_, ts, _ := newServer(t)
	code, body := do(t, ts, http.MethodGet, "/api/v1/audit/verify", nil, true)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d", code)
	}
	if body["supported"] != false {
		t.Errorf("内存实现应报 supported=false，实际 %v", body)
	}
	if s, _ := body["reason"].(string); s == "" {
		t.Error("应说明为什么不能校验")
	}
}

// TestAuditVerifyRealLog 真的落盘时：写入若干条后校验应通过，
// 手工改一行后同一端点必须报断点。
func TestAuditVerifyRealLog(t *testing.T) {
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

	// 造两条事件：直接经由审计实现写入，内容与执行门产生的同构。
	log.Record(service.AuditEvent{
		ID: "e1", Actor: "human:local", Action: "query.execute",
		Risk: policy.R0, Friction: policy.F0, Outcome: "ok",
	})
	log.Record(service.AuditEvent{
		ID: "e2", Actor: "human:local", Action: "query.execute",
		Risk: policy.R1, Friction: policy.F2, Outcome: "denied",
	})

	code, body := do(t, ts, http.MethodGet, "/api/v1/audit/verify", nil, true)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d %v", code, body)
	}
	if body["supported"] != true || body["ok"] != true {
		t.Fatalf("应报完整: %v", body)
	}
	if n, _ := body["events"].(float64); n != 2 {
		t.Errorf("事件数 = %v，期望 2", body["events"])
	}
}
