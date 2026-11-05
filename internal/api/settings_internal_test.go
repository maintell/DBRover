package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/infra/settings"
	"github.com/maintell/DBRover/internal/service"
)

func newSettingsServer(t *testing.T) (*Server, settings.Store) {
	t.Helper()
	s := New("test-token", nil, &service.SliceAudit{})
	st := settings.NewMemStore(settings.Defaults())
	s.UseSettings(st, func() {})
	return s, st
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-DBRover-Token", "test-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSettingsGetAndPatch(t *testing.T) {
	s, _ := newSettingsServer(t)
	h := s.Handler()

	rec := do(t, h, http.MethodGet, "/api/v1/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d", rec.Code)
	}
	var got settings.Settings
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Theme != "dark" || got.FontSize != 13 {
		t.Fatalf("默认值不对: %+v", got)
	}

	rec = do(t, h, http.MethodPatch, "/api/v1/settings", `{"theme":"light","font_size":16,"agent_model":"m"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /settings = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Theme != "light" || got.FontSize != 16 || got.AgentModel != "m" {
		t.Fatalf("PATCH 没生效: %+v", got)
	}
	// 重读确认持久化到 store。
	rec = do(t, h, http.MethodGet, "/api/v1/settings", "")
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Theme != "light" {
		t.Fatal("PATCH 后没读回新值")
	}
}

func TestSettingsRejectsInvalid(t *testing.T) {
	s, _ := newSettingsServer(t)
	h := s.Handler()
	for _, body := range []string{
		`{"theme":"neon"}`,
		`{"font_size":99}`,
		`{"editor_tab_size":1}`,
		`{"agent_context_tokens":-1}`,
		`{"not_a_field":1}`,
	} {
		rec := do(t, h, http.MethodPatch, "/api/v1/settings", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 应 400，实际 %d", body, rec.Code)
		}
	}
}

// TestSettingsNeverEchoesAPIKey 是安全红线：响应里不能出现写进去的 key。
func TestSettingsNeverEchoesAPIKey(t *testing.T) {
	s, _ := newSettingsServer(t)
	h := s.Handler()
	const key = "sk-super-secret-value"
	rec := do(t, h, http.MethodPatch, "/api/v1/settings", `{"api_key":"`+key+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH with api_key = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), key) {
		t.Fatalf("响应回显了 API Key: %s", rec.Body.String())
	}
}

func TestSettingsRequiresToken(t *testing.T) {
	s, _ := newSettingsServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 访问 /settings 应 401，实际 %d", rec.Code)
	}
}

// TestSettingsPatchBodyMatchesStore 双向比对 PATCH 请求体契约与 settings.Patch 的 json 标签。
// 任一方向漂移都会让客户端「按契约生成的字段被拒」或「有字段没写进契约」。
func TestSettingsPatchBodyMatchesStore(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(SpecJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	op := doc.Paths["/api/v1/settings"]["patch"]
	props := op.RequestBody.Content["application/json"].Schema.Properties
	if len(props) == 0 {
		t.Fatal("openapi.json 里 /settings patch 没有 requestBody.properties")
	}
	specFields := map[string]bool{}
	for f := range props {
		specFields[f] = true
	}
	want := map[string]bool{}
	typ := reflect.TypeOf(settings.Patch{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		want[strings.Split(tag, ",")[0]] = true
	}
	if len(want) < 7 {
		t.Fatalf("只从 settings.Patch 认出 %d 个字段，解析恐怕失效", len(want))
	}
	for f := range want {
		if !specFields[f] {
			t.Errorf("settings.Patch 的字段 %q 不在 openapi 契约里", f)
		}
	}
	for f := range specFields {
		if !want[f] {
			t.Errorf("openapi 声明了 %q，settings.Patch 里没有（真发过去会被拒）", f)
		}
	}
}
