package host

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/maintell/DBRover/internal/api"
)

// BootstrapPath 是桌面窗口取令牌的脚本路径。serve 模式下由 api.Handler 返回空值，
// 桌面模式下由 DesktopHandler 拦截并注入进程内令牌。
const BootstrapPath = "/bootstrap.js"

// BootstrapTokenJS 生成注入脚本的内容。token 只在内存里拼接，不打印、不落盘。
func BootstrapTokenJS(token string) string {
	return fmt.Sprintf("window.DBRoverBootstrapToken=%q;", token)
}

// DesktopHandler 把 api.Server 包装成 Wails 进程内可直接用的 Handler。
//
// 它不做网络监听：Wails 的 AssetServer{Handler: ...} 会把 WebView2 的请求在进程内
// 交给这里。两点包装：
//   - GET BootstrapPath 返回注入令牌的脚本（桌面不打印 URL，令牌这样进窗口）；
//   - WebView 伪造的请求里 Host 可能与 Origin 不一致，把 r.Host 对齐到 Origin 的
//     host，让 api.originAllowed 的同源判定成立。桌面没有网络入口，所有请求都来自
//     本窗口，这不是放宽校验。
func DesktopHandler(s *api.Server, token string) http.Handler {
	inner := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == BootstrapPath {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(BootstrapTokenJS(token)))
			return
		}
		r2 := r.Clone(r.Context())
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err == nil && u.Host != "" {
				r2.Host = u.Host
			}
		}
		inner.ServeHTTP(w, r2)
	})
}
