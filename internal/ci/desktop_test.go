package ci

import (
	"os"
	"path/filepath"
	"regexp"
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

// TestDesktopBuildUsesProductionTag 守住 Wails v2 的编译期构建标签。
//
// Wails v2.10.2 的 internal/app/app_default_windows.go 约束是
// 「!dev && !production && !bindings && windows」：不带任何 tag 时，Windows 上
// 链接进来的 CreateApp 是「弹一个 build tags 错误框就退出」的 stub，go build
// 照样返回 0、CI 照样绿，用户拿到的却是个假 exe。带 -tags production 才会
// 链接 app_production.go 的真实 CreateApp，并把内嵌的 runtime JS 一起带上。
//
// 这条门禁把「CI 构建命令」与「本地构建说明」两处都钉住：漏了 tag、或只改
// 一处忘了另一处，都会红。判据用正则带边界，避免 -tags production-extra 之类
// 的子串假绿。
func TestDesktopBuildUsesProductionTag(t *testing.T) {
	root := repoRoot(t)
	read := func(rel ...string) string {
		b, err := os.ReadFile(filepath.Join(append([]string{root}, rel...)...))
		if err != nil {
			t.Fatalf("读 %v: %v", rel, err)
		}
		return string(b)
	}

	release := read(".github", "workflows", "release.yml")
	readme := read("cmd", "dbrover-desktop", "README.md")
	for name, doc := range map[string]string{"release.yml": release, "desktop README": readme} {
		if len(doc) < 200 {
			t.Errorf("%s 只读到 %d 字节，扫描恐怕失效", name, len(doc))
		}
	}

	// 桌面 exe 的构建命令必须带 -tags production（tag 后跟边界，不接受更长单词）。
	wantTag := regexp.MustCompile(`-tags\s+production\b`)
	for name, doc := range map[string]string{"release.yml": release, "desktop README": readme} {
		if !wantTag.MatchString(doc) {
			t.Errorf("%s 的桌面 go build 没有 -tags production，产物会是弹错误框的 stub", name)
		}
	}

	// release.yml 里桌面 job 的 go build 行必须自己带 tag：只看文件里某处出现过
	// -tags production 会假绿（比如写在注释或其他 job 里）。
	desktopBuild := regexp.MustCompile(`(?m)^\s*go build [^\n]*-tags\s+production\b[^\n]*dbrover-desktop\.exe`)
	if !desktopBuild.MatchString(release) {
		t.Error("release.yml 里产出 dbrover-desktop.exe 的那条 go build 没有 -tags production")
	}
}

// TestDesktopEmbedsWebView2Bootstrapper 守住「桌面 exe 能独立运行」：目标机器没预装
// 或版本过旧的 WebView2 Runtime 时，默认策略（download.go）会弹英文框要求联网下载，
// 用户取消/无外网就起不来。Wails 官方 -webview2 embed 对应 build tag wv2runtime.embed，
// 把官方 MicrosoftEdgeWebview2Setup.exe 编进 exe，缺运行时时本地直接安装、不依赖网络。
// 裸 go build 不带该 tag 就退回 download 策略，CI 照样绿但桌面包不自包含。
func TestDesktopEmbedsWebView2Bootstrapper(t *testing.T) {
	root := repoRoot(t)
	read := func(rel ...string) string {
		b, err := os.ReadFile(filepath.Join(append([]string{root}, rel...)...))
		if err != nil {
			t.Fatalf("读 %v: %v", rel, err)
		}
		return string(b)
	}
	release := read(".github", "workflows", "release.yml")
	readme := read("cmd", "dbrover-desktop", "README.md")
	for name, doc := range map[string]string{"release.yml": release, "desktop README": readme} {
		if len(doc) < 200 {
			t.Errorf("%s 只读到 %d 字节，扫描恐怕失效", name, len(doc))
		}
		// 构建命令必须同时带 production 与 wv2runtime.embed（带边界，避免子串假绿）。
		prodTag := regexp.MustCompile(`-tags\s+[^\n]*\bproduction\b`)
		embedTag := regexp.MustCompile(`-tags\s+[^\n]*\bwv2runtime\.embed\b`)
		if !prodTag.MatchString(doc) {
			t.Errorf("%s 的桌面 go build 缺 production tag", name)
		}
		if !embedTag.MatchString(doc) {
			t.Errorf("%s 的桌面 go build 缺 wv2runtime.embed tag（缺 WebView2 的机器会弹框要求联网下载，不能独立运行）", name)
		}
	}
}
