package api_test

import (
	"net/http"
	"net/url"
	"testing"
)

// TestOriginGuard 覆盖 docs/10 威胁模型 T2：
// token 可能被带进浏览器上下文，此时任意网页都能跨站发起请求。
func TestOriginGuard(t *testing.T) {
	_, ts, _ := newServer(t)

	send := func(origin string) int {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/connections", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-DBRover-Token", testToken)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	if code := send(""); code != http.StatusOK {
		t.Errorf("无 Origin（curl/桌面壳）应放行, 实际 %d", code)
	}

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("解析测试服务地址: %v", err)
	}
	sameOrigin := "http://" + u.Host
	if code := send(sameOrigin); code != http.StatusOK {
		t.Errorf("同源 %s 应放行, 实际 %d", sameOrigin, code)
	}

	for _, bad := range []string{"http://evil.example", "null", "http://127.0.0.1:1"} {
		if code := send(bad); code != http.StatusForbidden {
			t.Errorf("Origin=%q 应返回 403, 实际 %d", bad, code)
		}
	}
}

// TestOriginGuardDoesNotCoverStaticAssets 静态资源不走 Origin 校验（页面本身无数据）。
func TestOriginGuardDoesNotCoverStaticAssets(t *testing.T) {
	_, ts, _ := newServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/style.css", nil)
	req.Header.Set("Origin", "http://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("静态资源应放行, 实际 %d", resp.StatusCode)
	}
}
