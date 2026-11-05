package host

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/api"
	"github.com/maintell/DBRover/internal/service"
)

func newDesktopRuntime(t *testing.T) (*api.Server, http.Handler) {
	t.Helper()
	s := api.New("", nil, &service.SliceAudit{})
	return s, DesktopHandler(s, s.Token())
}

func TestDesktopBootstrapInjectsServerToken(t *testing.T) {
	s, h := newDesktopRuntime(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, BootstrapPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("bootstrap 必须禁用缓存")
	}
	body := rec.Body.String()
	if !strings.Contains(body, s.Token()) {
		t.Errorf("bootstrap 没有注入 api.Server 的 token: %q", body)
	}
	if !strings.Contains(body, "DBRoverBootstrapToken") {
		t.Errorf("bootstrap 没写全局量: %q", body)
	}
}

// TestDesktopBootstrapNeverLogsToken 令牌不得出现在别处（页面/静态资源）里。
func TestDesktopBootstrapNeverLogsToken(t *testing.T) {
	_, h := newDesktopRuntime(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), "DBRoverBootstrapToken") {
		t.Fatal("index.html 里不该出现令牌")
	}
}

// TestDesktopAlignsHostToOrigin 保证带 Origin 的 API 请求能过同源校验。
func TestDesktopAlignsHostToOrigin(t *testing.T) {
	_, h := newDesktopRuntime(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Host = "wails.localhost"
	req.Header.Set("Origin", "http://wails.localhost")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("对齐后 /health 应 200，实际 %d", rec.Code)
	}
}

// TestDesktopAPIStillRequiresToken 包装层不能顺手把鉴权放开。
func TestDesktopAPIStillRequiresToken(t *testing.T) {
	_, h := newDesktopRuntime(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/connections", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 访问 API 应 401，实际 %d", rec.Code)
	}
}
