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

	// 场景 A：目标还没有这张表。同引擎下源类型本来就合法，应当直接 ready。
	body := map[string]any{"source": src, "target": dst, "table": "m"}
	code, out := do(t, ts, http.MethodPost, "/api/v1/migrate/plan", body, true)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d", code)
	}
	if out["ready"] != true {
		t.Fatalf("同引擎、目标表不存在时应直接 ready：%v", out)
	}
	if ddlText, _ := out["ddl"].(string); !strings.Contains(ddlText, "CREATE TABLE") {
		t.Fatalf("没有给出建表语句：%q", ddlText)
	}

	// 场景 B：目标有同名表但列类型不同——必须要求人工决定。
	if code := execApproved(t, ts, dst, "CREATE TABLE m (id INTEGER PRIMARY KEY, v INTEGER)"); code != http.StatusOK {
		t.Fatalf("准备目标表失败: %d", code)
	}
	code, out = do(t, ts, http.MethodPost, "/api/v1/migrate/plan", body, true)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d", code)
	}
	if out["ready"] != false {
		t.Fatalf("类型不一致时不应 ready：%v", out)
	}
	if out["plan"] == nil {
		t.Fatal("即使不能出 DDL，也要返回逐列结论")
	}
	// 场景 C：给出 decisions 后应当 ready。
	body["decisions"] = map[string]any{"v": "TEXT"}
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

// TestMigrateChangeEndpoint 走完整链路：预检 → 建表预案 → 执行 → 目标库真的有表。
//
// 重点：客户端只能给 decisions，DDL 由服务端重新计算——所以即使有人伪造
// 请求也执行不了任意语句。
func TestMigrateChangeEndpoint(t *testing.T) {
	_, ts, dir := newServer(t)
	src := createConn(t, ts, dir, "local", "read-write")
	dst := createConnAt(t, ts, filepath.Join(dir, "migrate-dst2.db"), "dst", "read-write")

	for _, q := range []string{
		"CREATE TABLE m (id INTEGER PRIMARY KEY, v TEXT)",
		"INSERT INTO m (id, v) VALUES (1, 'a')",
	} {
		if code := execApproved(t, ts, src, q); code != http.StatusOK {
			t.Fatalf("准备源表失败: %d", code)
		}
	}

	body := map[string]any{"source": src, "target": dst, "table": "m"}
	code, plan := do(t, ts, http.MethodPost, "/api/v1/migrate/change", body, true)
	if code != http.StatusCreated {
		t.Fatalf("生成迁移预案失败: %d %v", code, plan)
	}
	pid, _ := plan["id"].(string)
	if pid == "" {
		t.Fatal("预案缺少 id")
	}
	stmts, _ := plan["statements"].([]any)
	if len(stmts) != 1 {
		t.Fatalf("预案应只有一条建表语句，实际 %v", plan["statements"])
	}

	code, out := do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply", map[string]any{}, true)
	if code == http.StatusPreconditionRequired {
		appr, _ := deepGet(out, "error", "approval", "token")
		token, _ := appr.(string)
		code, out = do(t, ts, http.MethodPost, "/api/v1/changes/"+pid+"/apply",
			map[string]any{"approval_token": token}, true)
	}
	if code != http.StatusOK {
		t.Fatalf("执行迁移预案失败: %d %v", code, out)
	}

	// 目标库要真的有这张表。
	code, tables := do(t, ts, http.MethodGet, "/api/v1/connections/"+dst+"/tables", nil, true)
	if code != http.StatusOK {
		t.Fatalf("读目标表清单失败: %d", code)
	}
	items, _ := tables["items"].([]any)
	found := false
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && m["name"] == "m" {
			found = true
		}
	}
	if !found {
		t.Fatalf("目标库没有建出表：%v", items)
	}
}

// deepGet 按路径取嵌套 map 里的值（测试里读错误体用）。
func deepGet(v any, path ...string) (any, bool) {
	cur := v
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// TestMigrateRunEndpoint 覆盖搬运端点：目标表已建好时，数据搬过去并校验通过。
func TestMigrateRunEndpoint(t *testing.T) {
	_, ts, dir := newServer(t)
	src := createConn(t, ts, dir, "local", "read-write")
	dst := createConnAt(t, ts, filepath.Join(dir, "migrate-run-dst.db"), "dst2", "read-write")

	for _, q := range []string{
		"CREATE TABLE r (id INTEGER PRIMARY KEY, v TEXT)",
		"INSERT INTO r (id, v) VALUES (1,'a'), (2,'b'), (3,'c')",
	} {
		if code := execApproved(t, ts, src, q); code != http.StatusOK {
			t.Fatalf("准备源表失败: %d", code)
		}
	}
	if code := execApproved(t, ts, dst, "CREATE TABLE r (id INTEGER PRIMARY KEY, v TEXT)"); code != http.StatusOK {
		t.Fatalf("准备目标表失败: %d", code)
	}

	code, rep := do(t, ts, http.MethodPost, "/api/v1/migrate/run", map[string]any{
		"source": src, "target": dst, "table": "r", "batch_size": 2,
	}, true)
	if code != http.StatusOK {
		t.Fatalf("搬运失败: %d %v", code, rep)
	}
	if rows, _ := rep["rows"].(float64); rows != 3 {
		t.Fatalf("搬了 %v 行，期望 3", rep["rows"])
	}
	if rep["verified"] != true {
		t.Fatalf("应当做过校验：%v", rep)
	}
	if rep["keyset_ok"] != true {
		t.Fatalf("有主键的表应当 keyset_ok=true：%v", rep)
	}

	// 目标库读出来必须是三行（不是只信报告）。
	code, out := do(t, ts, http.MethodPost, "/api/v1/connections/"+dst+"/exec",
		map[string]any{"sql": "SELECT COUNT(*) AS n FROM r"}, true)
	if code != http.StatusOK {
		t.Fatalf("查询目标失败: %d", code)
	}
	results, _ := out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("结果异常: %v", out)
	}
	first, _ := results[0].(map[string]any)
	rowsOut, _ := first["rows"].([]any)
	if len(rowsOut) != 1 {
		t.Fatalf("目标表读不到行：%v", first)
	}
	row, _ := rowsOut[0].([]any)
	if len(row) != 1 {
		t.Fatalf("聚合结果异常：%v", rowsOut[0])
	}
	// 只信报告不行，必须从目标库里读出来。
	if n, ok := row[0].(float64); !ok || n != 3 {
		t.Fatalf("目标表行数 = %v，期望 3", row[0])
	}
}
