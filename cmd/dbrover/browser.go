package main

import (
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// browserCommand 返回「用系统默认浏览器打开这个 URL」该执行什么。
//
// 为什么不用第三方库：这件事本质上就是一次进程调用，各平台各一条命令。
// 引一个库只会多一个依赖面，而这段逻辑不到二十行、还能被单测覆盖。
//
// 为什么不用 "cmd /c start"：它会把 URL 里的 & 当成命令分隔符（界面地址带 #t=
// 一般没事，但 token 或参数里出现 & 就会截断），而且多经过一层 shell 解析。
// rundll32 url.dll 是 Windows 上最直接的做法。
func browserCommand(goos, url string) (name string, args []string, err error) {
	switch goos {
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}, nil
	case "darwin":
		return "open", []string{url}, nil
	case "linux", "freebsd", "openbsd", "netbsd":
		return "xdg-open", []string{url}, nil
	default:
		return "", nil, fmt.Errorf("这个平台没有已知的打开浏览器方式: %s", goos)
	}
}

// openBrowser 尽力打开浏览器。
//
// 打不开不算致命：地址已经打印在控制台，用户自己复制粘贴即可。
// 所以这里只返回错误让调用方决定是否提示，不 panic、不退出。
func openBrowser(url string) error {
	name, args, err := browserCommand(runtime.GOOS, url)
	if err != nil {
		return err
	}
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	// 不 Wait：浏览器是长驻进程，等它等于把 CLI 挂住。
	// 这里显式放手，让它自己跑。
	_ = cmd.Process.Release()
	return nil
}

// waitForServer 等服务真正可连上再开浏览器。
//
// 不这么做的话，浏览器常常比监听更快就绪，用户看到的是一个连接被拒的页面，
// 然后以为启动失败了——这类「其实只是慢半拍」的体验最容易误伤信任。
func waitForServer(baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	health := strings.TrimRight(baseURL, "/") + "/api/v1/health"
	for {
		resp, err := client.Get(health)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("服务在 %v 内没有就绪", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
