package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// desktopDir 返回桌面壳 module 目录。
func desktopDir(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	dir := filepath.Join(root, "cmd", "dbrover-desktop")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("找不到桌面壳目录: %s", dir)
	}
	return dir
}

// TestDesktopModuleIsIsolated 守住 ADR-0017 `7 的构建隔离：桌面壳必须是独立
// module、pin 住 Wails v2、用 replace 指回仓库根。任何一条被改掉，根 module 的
// go test ./... / 五平台交叉编译都会被 Wails 拖住，或者 CI 产物拉到别的版本。
func TestDesktopModuleIsIsolated(t *testing.T) {
	dir := desktopDir(t)
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	mod := string(b)
	for _, want := range []string{
		"module github.com/maintell/DBRover/cmd/dbrover-desktop",
		"github.com/wailsapp/wails/v2 v2.10.2",
		"replace github.com/maintell/DBRover => ../..",
	} {
		if !strings.Contains(mod, want) {
			t.Errorf("桌面壳 go.mod 缺少 %q", want)
		}
	}
	// 根 go.mod 绝不能引入 Wails：那是免 CGO 交叉编译的前提。
	rootMod, err := os.ReadFile(filepath.Join(repoRoot(t), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rootMod), "wailsapp/wails") {
		t.Error("根 go.mod 引入了 Wails，会拖垮 CGO_ENABLED=0 的交叉编译与 go test ./...")
	}
}

// TestDesktopShellUsesInProcessHandler 守住「桌面不监听端口」：入口必须把
// host.DesktopHandler 交给 AssetServer.Handler，且 Assets 留空。写成另起一个
// loopback 服务就违背了 ADR-0017 的安全边界。
func TestDesktopShellUsesInProcessHandler(t *testing.T) {
	dir := desktopDir(t)
	b, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		"host.New(host.Options{})",
		"host.DesktopHandler(rt.Server, token)",
		"&assetserver.Options{Handler: handler}",
		"wails.Run(appOpts)",
		"defer rt.Close()",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("桌面入口缺少 %q", want)
		}
	}
	for _, bad := range []string{"ListenAndServe", "s.Serve(", "127.0.0.1:"} {
		if strings.Contains(src, bad) {
			t.Errorf("桌面入口不该监听端口（出现 %q）", bad)
		}
	}
}

// TestDesktopBootstrapWiring 守住令牌注入链路的两端：界面读注入量、服务端提供
// bootstrap 静态资源。任一处断掉，桌面窗口会卡在令牌门。
func TestDesktopBootstrapWiring(t *testing.T) {
	root := repoRoot(t)
	index, err := os.ReadFile(filepath.Join(root, "internal", "webui", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "/bootstrap.js") {
		t.Error("index.html 没有在 app.js 之前引入 /bootstrap.js")
	}
	appjs, err := os.ReadFile(filepath.Join(root, "internal", "webui", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(appjs), "window.DBRoverBootstrapToken") {
		t.Error("app.js 没有读桌面注入的令牌")
	}
	srv, err := os.ReadFile(filepath.Join(root, "internal", "api", "server.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(srv), "GET /bootstrap.js") {
		t.Error("serve 模式没有提供空的 /bootstrap.js，桌面链路会 404")
	}
	ui2, err := os.ReadFile(filepath.Join(root, "internal", "webui", "ui2.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"API.settings", "openSettings()", "openConnections()", "renderTabs()"} {
		if !strings.Contains(string(ui2), want) {
			t.Errorf("ui2.js 缺少交互接线 %q", want)
		}
	}
}
