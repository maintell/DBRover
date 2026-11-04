package api_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigratePlanEndpoint 覆盖迁移预检端点：目标表存在且结构一致时给出 DDL，
// 目标表不存在时明确说「还需人工决定」而不是硬编一个类型出来。
func TestMigratePlanEndpoint(t *testing.T) {
	_, ts, dir := newServer(t)
	src := createConn(t, ts, dir, "local", "read-write")
	// createConn 固定用 dir/api.db，两次调用会共用同一个库——那样「目标表不存在」
	// 的场景根本造不出来。这里手工指定另一个文件。
	dst := createConnAt(t, ts, filepath.Join(dir, "migrate-dst.db"), "dst", "read-write")

	for _, q := range []string{
		"CREATE TABLE m (id INTEGER PRIMARY KEY, v TEXT)",
		"INSERT INTO m (id, v) VALUES (1, 'a')",
	} {
		if code := execApproved(t, ts, src, q); code != http.StatusOK {
			t.Fatalf("准备源表失败: %d", code)
		}
	}

	// 目标还没有这张表：只能给出需要人工决定的结论。
	body := map[string]any{"source": src, "target": dst, "table": "m"}
	code, out := do(t, ts, http.MethodPost, "/api/v1/migrate/plan", body, true)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d", code)
	}
	if out["ready"] != false {
		t.Fatalf("目标表不存在时不应 ready：%v", out)
	}
	if out["plan"] == nil {
		t.Fatal("即使不能出 DDL，也要返回逐列结论")
	}

	// 在目标建同一张表后再问：应当 ready=true 并给出 DDL。
	for _, q := range []string{
		"CREATE TABLE m (id INTEGER PRIMARY KEY, v TEXT)",
	} {
		if code := execApproved(t, ts, dst, q); code != http.StatusOK {
			t.Fatalf("准备目标表失败: %d", code)
		}
	}
	code, out = do(t, ts, http.MethodPost, "/api/v1/migrate/plan", body, true)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d", code)
	}
	if out["ready"] != true {
		t.Fatalf("结构一致时应当 ready：%v", out)
	}
	ddlText, _ := out["ddl"].(string)
	if !strings.Contains(ddlText, "CREATE TABLE") || !strings.Contains(ddlText, `"id"`) {
		t.Fatalf("DDL 不对：%q", ddlText)
	}
}
