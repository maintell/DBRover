package settings

import (
	"errors"
	"os"
	"strings"
	"testing"
)

type memKeys struct {
	v   string
	has bool
}

func (m *memKeys) Store(_, _, value string) error { m.v = value; m.has = true; return nil }
func (m *memKeys) Load(_, _ string) (string, error) {
	if !m.has {
		return "", errors.New("not found")
	}
	return m.v, nil
}
func (m *memKeys) Delete(_, _ string) error { m.v = ""; m.has = false; return nil }

func strp(s string) *string { return &s }
func intp(n int) *int       { return &n }

func TestFileStoreDefaultsWithoutFile(t *testing.T) {
	st, err := newFileStore(t.TempDir()+"/settings.json", &memKeys{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Theme != "dark" || got.FontSize != 13 || got.EditorTabSize != 2 {
		t.Fatalf("默认值不对: %+v", got)
	}
	if got.APIKeyConfigured {
		t.Error("没配 key 却说已配置")
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	path := t.TempDir() + "/settings.json"
	st, err := newFileStore(path, &memKeys{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(Patch{Theme: strp("light"), FontSize: intp(16), AgentModel: strp("deepseek-chat")}); err != nil {
		t.Fatal(err)
	}
	st2, err := newFileStore(path, &memKeys{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Theme != "light" || got.FontSize != 16 || got.AgentModel != "deepseek-chat" {
		t.Fatalf("设置没持久化: %+v", got)
	}
}

func TestFileStoreRejectsInvalid(t *testing.T) {
	st, _ := newFileStore(t.TempDir()+"/s.json", &memKeys{})
	for _, p := range []Patch{
		{Theme: strp("neon")},
		{FontSize: intp(99)},
		{EditorTabSize: intp(1)},
		{AgentContextTokens: intp(-1)},
	} {
		if _, err := st.Update(p); err == nil {
			t.Errorf("非法设置 %+v 应被拒绝", p)
		}
	}
}

// TestAPIKeyNeverInFile 是安全红线：把 key 写进明文文件是最容易犯也最贵的错。
func TestAPIKeyNeverInFile(t *testing.T) {
	path := t.TempDir() + "/settings.json"
	keys := &memKeys{}
	st, _ := newFileStore(path, keys)
	const key = "sk-this-must-never-appear-in-json"
	if _, err := st.Update(Patch{APIKey: strp(key)}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), key) {
		t.Fatalf("API Key 出现在设置文件里: %s", b)
	}
	if strings.Contains(strings.ToLower(string(b)), "api_key") {
		t.Fatalf("设置文件里出现了凭据字段名: %s", b)
	}
	if !keys.has || keys.v != key {
		t.Fatal("key 没进凭据库")
	}
	got, _ := st.Load()
	if !got.APIKeyConfigured {
		t.Fatal("存了 key 却报未配置")
	}
}

func TestAPIKeyEmptyClears(t *testing.T) {
	keys := &memKeys{}
	st, _ := newFileStore(t.TempDir()+"/s.json", keys)
	st.Update(Patch{APIKey: strp("abc")})
	if _, err := st.Update(Patch{APIKey: strp("")}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Load()
	if got.APIKeyConfigured {
		t.Fatal("清空后仍报已配置")
	}
}
