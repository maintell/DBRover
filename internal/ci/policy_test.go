package ci_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func workflowDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", ".github", "workflows")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("找不到 workflow 目录: %s", dir)
	}
	return dir
}

// readWorkflow 读取并去掉整行注释。
// 去注释是必须的：文件里的说明性注释会合法地出现 "pull_request" 这类词，
// 直接匹配会误判。
func readWorkflow(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("读取 %s: %v", name, err)
	}
	var out strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

func mustContain(t *testing.T, name, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("%s 应包含 %q", name, want)
	}
}

func mustNotContain(t *testing.T, name, body, unwanted string) {
	t.Helper()
	if strings.Contains(body, unwanted) {
		t.Errorf("%s 不应包含 %q", name, unwanted)
	}
}

// TestReleaseOnlyOnTag 守住「发版只由 tag 触发」。
func TestReleaseOnlyOnTag(t *testing.T) {
	dir := workflowDir(t)
	body := readWorkflow(t, dir, "release.yml")

	mustContain(t, "release.yml", body, "tags:")
	mustContain(t, "release.yml", body, "'v*'")
	mustNotContain(t, "release.yml", body, "pull_request")
	mustNotContain(t, "release.yml", body, "branches:")
}

// TestReleaseHasSinglePipeline 守住「一条 tag 只产生一条流水线」。
func TestReleaseHasSinglePipeline(t *testing.T) {
	dir := workflowDir(t)
	body := readWorkflow(t, dir, "release.yml")

	mustContain(t, "release.yml", body, "concurrency:")
	mustContain(t, "release.yml", body, "group: release")
	// 并发组不能按事件区分，否则 tag 推送与手动派发会各成一组、并行抢同一批资产。
	mustNotContain(t, "release.yml", body, "github.event_name")
	mustNotContain(t, "release.yml", body, "github.ref }}")
}

// TestReleaseRunsNoTests 守住「检查在本地，runner 只编译与发布」。
func TestReleaseRunsNoTests(t *testing.T) {
	dir := workflowDir(t)
	body := readWorkflow(t, dir, "release.yml")

	mustNotContain(t, "release.yml", body, "go test")
	mustNotContain(t, "release.yml", body, "gofmt")
	mustNotContain(t, "release.yml", body, "go vet")
	mustNotContain(t, "release.yml", body, "golangci-lint")

	// 发布产物必须是免 CGO 的交叉编译。
	mustContain(t, "release.yml", body, "CGO_ENABLED")
	mustContain(t, "release.yml", body, "GOOS: ${{ matrix.goos }}")
	mustContain(t, "release.yml", body, "GOARCH: ${{ matrix.goarch }}")
}

// TestCIRunsTestsWithoutPackaging 守住「PR 只跑测试，不启动打包」。
func TestCIRunsTestsWithoutPackaging(t *testing.T) {
	dir := workflowDir(t)
	body := readWorkflow(t, dir, "ci.yml")

	mustContain(t, "ci.yml", body, "pull_request")
	mustContain(t, "ci.yml", body, "go test ./...")
	mustContain(t, "ci.yml", body, "go test -race ./...")
	mustContain(t, "ci.yml", body, "go vet ./...")

	mustNotContain(t, "ci.yml", body, "action-gh-release")
	mustNotContain(t, "ci.yml", body, "tags:")
	mustNotContain(t, "ci.yml", body, "cmd/dbrover")
}

// TestOnlyOneReleasePath 保证不存在第二条发版路径。
func TestOnlyOneReleasePath(t *testing.T) {
	dir := workflowDir(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录: %v", err)
	}

	var releases []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		body := readWorkflow(t, dir, e.Name())
		if strings.Contains(body, "action-gh-release") || strings.Contains(body, "goreleaser") {
			releases = append(releases, e.Name())
		}
	}

	if len(releases) != 1 {
		t.Fatalf("应只有一条发版流水线，实际 %d 条: %v", len(releases), releases)
	}
	if releases[0] != "release.yml" {
		t.Errorf("发版流水线应为 release.yml，实际 %s", releases[0])
	}
}

// TestWorkflowsShape 做最基本的结构检查，避免 YAML 被改成明显不成立的样子。
func TestWorkflowsShape(t *testing.T) {
	dir := workflowDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("workflow 目录为空")
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		body := readWorkflow(t, dir, e.Name())
		mustContain(t, e.Name(), body, "name:")
		mustContain(t, e.Name(), body, "on:")
		mustContain(t, e.Name(), body, "jobs:")
		mustContain(t, e.Name(), body, "runs-on:")
		mustContain(t, e.Name(), body, "actions/checkout@")
	}
}
