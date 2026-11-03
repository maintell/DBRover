package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maintell/DBRover/internal/api"
	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// fakeKV 是给接口层门禁用例用的假 key-value 连接器。
//
// 用假实现而不是真 Redis：这里要验的是「哪些命令被拒绝」，那是 api 的判断，
// 与 Redis 是否可用无关；真机验收由 scripts/accept 覆盖。
type fakeKV struct {
	lastCommand string
	scanned     int
}

func (f *fakeKV) Engine() model.Engine                             { return model.EngineRedis }
func (f *fakeKV) Capabilities() model.Capabilities                 { return model.Capabilities{} }
func (f *fakeKV) Open(context.Context, driver.ConnectConfig) error { return nil }
func (f *fakeKV) Close() error                                     { return nil }
func (f *fakeKV) Health(context.Context) (*driver.HealthInfo, error) {
	return &driver.HealthInfo{Version: "fake-redis"}, nil
}
func (f *fakeKV) Scan(context.Context, driver.KVScanOptions) (*driver.KVPage, error) {
	f.scanned++
	return &driver.KVPage{Keys: []string{"k1", "k2"}}, nil
}
func (f *fakeKV) Inspect(context.Context, string) (*driver.KVEntry, error) {
	return &driver.KVEntry{Key: "k1", Type: driver.KVString, TTL: -1, Encoding: "embstr"}, nil
}
func (f *fakeKV) Value(context.Context, string, driver.KVValueOptions) (*driver.KVValue, error) {
	s := "hello"
	return &driver.KVValue{Key: "k1", Type: driver.KVString, String: &s}, nil
}
func (f *fakeKV) Mutate(context.Context, driver.KVMutation) (*driver.KVResult, error) {
	return &driver.KVResult{Changed: 1}, nil
}
func (f *fakeKV) Command(_ context.Context, name string, _ []any) (*driver.KVResult, error) {
	f.lastCommand = name
	return &driver.KVResult{Rows: [][]any{{"ok"}}, Changed: -1}, nil
}

// newKVServer 起一个只返回假 KV 连接器的服务。
func newKVServer(t *testing.T) (*fakeKV, *httptest.Server) {
	t.Helper()
	kv := &fakeKV{}
	opener := func(model.Engine, driver.ConnectConfig) (any, error) { return kv, nil }
	s := api.New(testToken, opener, &service.SliceAudit{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		s.Close()
	})
	return kv, ts
}

// TestKVEndpointsAndPolicyGates 覆盖 key-value 端点与三条门禁。
//
// 门禁是这一批的重点：命令模式如果什么都放行，等于给了一个绕过只读与审批的
// 后门；如果什么都不放行（第一版实现就犯了这错，连 DBSIZE 都拒），
// 只读连接的命令模式就整个作废。
func TestKVEndpointsAndPolicyGates(t *testing.T) {
	kv, ts := newKVServer(t)

	code, body := do(t, ts, http.MethodPost, "/api/v1/connections", map[string]any{
		"name": "r", "engine": "redis", "host": "127.0.0.1", "port": 6379,
		"catalog": "0", "environment": "test", "access_mode": "read-only",
	}, true)
	if code != http.StatusCreated {
		t.Fatalf("建连 status = %d, body=%v", code, body)
	}
	id, _ := body["id"].(string)

	// 1) 遍历 keys
	code, body = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/kv/keys?count=5", nil, true)
	if code != http.StatusOK {
		t.Fatalf("kv/keys status = %d, body=%v", code, body)
	}
	if keys, _ := body["keys"].([]any); len(keys) != 2 {
		t.Errorf("keys = %v, 期望 2 条", body["keys"])
	}

	// 2) 看一个 key + 读值
	if code, body = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/kv/entry?key=k1", nil, true); code != http.StatusOK {
		t.Fatalf("kv/entry status = %d, body=%v", code, body)
	}
	if code, body = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/kv/value?key=k1", nil, true); code != http.StatusOK {
		t.Fatalf("kv/value status = %d, body=%v", code, body)
	}

	// 3) 只读命令放行（第一版曾把 DBSIZE 也拒掉，这条就是防它回退）
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/kv/command",
		map[string]any{"name": "DBSIZE"}, true)
	if code != http.StatusOK {
		t.Fatalf("只读命令应放行, status = %d, body=%v", code, body)
	}
	if kv.lastCommand != "DBSIZE" {
		t.Errorf("命令没有真正下发: %q", kv.lastCommand)
	}

	// 4) KEYS / DEBUG 直接拒（有等价替代或调试后门）
	for _, name := range []string{"KEYS", "DEBUG"} {
		code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/kv/command",
			map[string]any{"name": name, "args": []any{"*"}}, true)
		if code != http.StatusForbidden {
			t.Fatalf("%s 应 403, 实际 %d, body=%v", name, code, body)
		}
		errObj, _ := body["error"].(map[string]any)
		if errObj["code"] != "POLICY_COMMAND_BLOCKED" {
			t.Errorf("%s code = %v", name, errObj["code"])
		}
	}

	// 5) 写命令在命令模式里一律拒（写路径要过审批与审计，尚未开放）
	code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+id+"/kv/command",
		map[string]any{"name": "SET", "args": []any{"k", "v"}}, true)
	if code != http.StatusForbidden {
		t.Fatalf("SET 应 403, 实际 %d, body=%v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "POLICY_WRITE_NOT_ENABLED" {
		t.Errorf("SET code = %v", errObj["code"])
	}
	if kv.lastCommand == "SET" {
		t.Error("被拒的写命令不该下发到驱动")
	}

	// 6) 关系型端点对 kv 连接返回 400，并指路到 kv 端点
	code, body = do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/tables", nil, true)
	if code != http.StatusBadRequest {
		t.Fatalf("kv 连接调 /tables 应 400, 实际 %d", code)
	}
	errObj, _ = body["error"].(map[string]any)
	if errObj["code"] != "CAPABILITY_UNSUPPORTED" {
		t.Errorf("code = %v", errObj["code"])
	}
	if hint, _ := errObj["hint"].(string); hint == "" {
		t.Error("应指路到 /kv/keys")
	}
}
