// Command dbrover-desktop 是 DBRover 的桌面形态：Wails v2 / WebView2 原生窗口。
//
// 它不是第二套实现：与 `dbrover serve` 共用同一份 `api.Server`、`internal/webui`、
// 执行门与审计（见 ADR-0017）。区别只在承载方式——窗口把 `s.Handler()` 交给
// Wails 的 AssetServer.Handler，请求在进程内派发，**不监听任何 TCP 端口**。
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/maintell/DBRover/internal/host"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "DBRover 桌面启动失败：", err)
		fmt.Fprintln(os.Stderr, "回退：命令行执行 `dbrover serve -open`，用浏览器打开同一份界面。")
		os.Exit(1)
	}
}

func run() error {
	// 装配与 serve 完全相同的内核；桌面默认不落审计/指标/锚点文件。
	rt, err := host.New(host.Options{})
	if err != nil {
		return err
	}
	defer rt.Close()

	token := rt.Server.Token()
	handler := host.DesktopHandler(rt.Server, token)

	appOpts := &options.App{
		Title:     "DBRover",
		Width:     1280,
		Height:    820,
		MinWidth:  960,
		MinHeight: 600,
		// Assets 留空：所有 GET 请求都交给 Handler（Wails 官方支持的用法，
		// v2.10.2 的 assetserver.Options 注释明确写了这一点）。
		AssetServer:      &assetserver.Options{Handler: handler},
		BackgroundColour: &options.RGBA{R: 18, G: 20, B: 26, A: 255},
		// 关窗即退出：wails.Run 返回后 defer 会关掉连接、审计与设置存储。
		OnShutdown: func(_ context.Context) {},
	}

	if err := wails.Run(appOpts); err != nil {
		return fmt.Errorf("窗口创建失败（可能缺少 WebView2 运行时）: %w", err)
	}
	return nil
}
