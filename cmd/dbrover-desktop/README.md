# dbrover-desktop

DBRover 的桌面形态（Wails v2 + Windows WebView2）。与 `dbrover serve` 共用同一份内核与界面，
详见 [ADR-0017](../../docs/adr/0017-desktop-shell-and-settings.md)。

## 为什么是独立 module

- 根 `go.mod` 不引入 Wails，保持 `CGO_ENABLED=0 go build ./...`、五平台 CLI 交叉编译与 `go test ./...` 不受影响。
- Go 会把嵌套 module 排除在根 `./...` 之外；本 module 通过 `replace` 指向仓库根，可直接复用 `internal/*` 包。
- Wails 在 Windows 上走 `go-webview2`（纯 Go syscall），不需要 CGO。

## 构建

```powershell
# 在 GitHub Actions 的 windows-latest runner 上执行（本地磁盘紧张，不在本机打包）
cd cmd/dbrover-desktop
$env:CGO_ENABLED = '0'
go mod tidy
go build -tags production,wv2runtime.embed -trimpath -ldflags "-s -w -H=windowsgui" -o ../../out/dbrover-desktop.exe .
```

> `-tags production` 不能省：Wails v2 在 Windows 上对「没带 dev/production/bindings 任一 build tag」
> 的构建会链接进一个弹「Wails applications will not build without the correct build tags」错误框
> 就退出的 stub（`internal/app/app_default_windows.go`），而 `go build` 仍返回 0。带 `production`
> 才会链接真实的 `CreateApp` 并把内嵌 runtime JS 一起编进去。
>
> `wv2runtime.embed` 也不能省：Wails v2 Windows 桌面应用依赖 **WebView2 Runtime**，Windows 11 默认带、
> 别的机器未必。不带该 tag 时走默认 `download` 策略（`internal/wv2installer/download.go`）——目标机器
> 没装或版本过旧时弹英文框要求**联网下载**官方 bootstrapper，用户取消或无外网就起不来。带
> `wv2runtime.embed`（等价于 `wails build -webview2 embed`）会把 Wails 自带的
> `MicrosoftEdgeWebview2Setup.exe`（`//go:embed`）编进 exe，缺运行时时本地直接安装、不依赖网络。
> 两个 tag 由 `internal/ci` 的 `TestDesktopBuildUsesProductionTag` /
> `TestDesktopEmbedsWebView2Bootstrapper` 双向钉住。
>
> 已装 ≥94.0.992.31 WebView2 的机器行为不变；首次在未装机器上运行会弹一次安装确认，装完即用。

产物由 `.github/workflows/release.yml` 在 `v*` tag 时产出，不在本地发版。

## 行为

- 启动：构造 `api.Server`（随机 token）→ 把 `s.Handler()` 交给 Wails `AssetServer.Handler` → 打开窗口。
- 令牌：进程内 `/bootstrap.js` 注入 `window.DBRoverBootstrapToken`，不打印、不落盘、不进日志。
- 请求：`/`、`/app.js`、`/ui2.js`、`/style.css`、`/api/v1/*` 全部进程内派发，无监听端口。
- 关窗：`wails.Run` 返回 → 关连接、审计、指标留存、设置存储 → 进程退出。
- 失败：WebView2 缺失 / 窗口创建失败时明确报错并提示 `dbrover serve -open` 回退，不静默白屏。
