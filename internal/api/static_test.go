package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestStaticAssetsArePublic(t *testing.T) {
	_, ts, _ := newServer(t)

	cases := []struct {
		path string
		want string
		body string
	}{
		{"/", "text/html", "DBRover"},
		{"/app.js", "text/javascript", "runSQL"},
		{"/style.css", "text/css", "--bg"},
	}

	for _, c := range cases {
		resp, err := http.Get(ts.URL + c.path)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d", c.path, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, c.want) {
			t.Errorf("GET %s Content-Type = %q, 期望包含 %q", c.path, ct, c.want)
		}
		if !strings.Contains(string(body), c.body) {
			t.Errorf("GET %s 内容不含 %q", c.path, c.body)
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s 应禁用缓存", c.path)
		}
	}
}

// TestApiStillRequiresToken 保证「静态资源公开」没有顺带把 API 放开。
func TestApiStillRequiresToken(t *testing.T) {
	_, ts, _ := newServer(t)

	resp, err := http.Get(ts.URL + "/api/v1/connections")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 访问 API status = %d, 期望 401", resp.StatusCode)
	}
}

// TestUnknownApiPathIs404 保证未匹配的 /api 路径不会被 SPA 兜底吞掉。
func TestUnknownApiPathIs404(t *testing.T) {
	_, ts, _ := newServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/nope", nil)
	req.Header.Set("X-DBRover-Token", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("未知 API 路径 status = %d, 期望 404", resp.StatusCode)
	}
}
