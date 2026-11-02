package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// getRaw 发一个带 token 的 GET 并返回原始响应，供导出这类非 JSON 响应使用。
func getRaw(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-DBRover-Token", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

func TestExportEndpointCSV(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	seedRows(t, ts, id, 3)

	resp, body := getRaw(t, ts.URL+"/api/v1/connections/"+id+"/export?table=s&format=csv")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, 期望 text/csv", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "s.csv") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("应有表头 + 3 行, 实际 %d 行: %q", len(lines), body)
	}
	if lines[0] != "id" {
		t.Errorf("表头 = %q, 期望 id", lines[0])
	}
}

func TestExportEndpointNDJSON(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	seedRows(t, ts, id, 2)

	resp, body := getRaw(t, ts.URL+"/api/v1/connections/"+id+"/export?table=s&format=ndjson&order="+
		`%7B%22column%22%3A%22id%22%2C%22desc%22%3Atrue%7D`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Errorf("Content-Type = %q", ct)
	}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 行, 实际 %d: %q", len(lines), body)
	}
	if !strings.Contains(lines[0], `"id":"2"`) {
		t.Errorf("倒序首行应是 id=2, 实际 %q", lines[0])
	}
}

// TestExportEndpointErrorsAreJSON 验证失败时返回结构化错误，而不是一个空文件配 200。
// 这是 lazyWriteWriter 存在的理由：写响应头必须推迟到真的写出数据。
func TestExportEndpointErrorsAreJSON(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")
	seedRows(t, ts, id, 1)

	cases := []struct {
		name string
		path string
		code string
	}{
		{"不支持的格式", "/export?table=s&format=xml", "EXPORT_FORMAT_UNSUPPORTED"},
		{"缺 table", "/export?format=csv", "REQUEST_INVALID"},
		{"筛选列不存在", "/export?table=s&where=" + "%5B%7B%22column%22%3A%22nope%22%2C%22op%22%3A%22eq%22%2C%22value%22%3A%221%22%7D%5D", "FILTER_INVALID"},
		{"表不存在", "/export?table=nosuch", "EXPORT_FAILED"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := do(t, ts, http.MethodGet, "/api/v1/connections/"+id+c.path, nil, true)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, 期望 400, body=%v", code, body)
			}
			errObj, _ := body["error"].(map[string]any)
			if errObj["code"] != c.code {
				t.Errorf("code = %v, 期望 %s", errObj["code"], c.code)
			}
		})
	}
}

// TestExportNeedsToken 确认导出不是免鉴权的旁路。
func TestExportNeedsToken(t *testing.T) {
	_, ts, dir := newServer(t)
	id := createConn(t, ts, dir, "local", "read-write")

	code, body := do(t, ts, http.MethodGet, "/api/v1/connections/"+id+"/export?table=s", nil, false)
	if code != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401, 实际 %d, body=%v", code, body)
	}
}
