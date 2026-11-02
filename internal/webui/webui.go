// Package webui 内嵌一个免构建的最小 Web 界面。
//
// 它是「临时界面」：目的是让能力可被点击验收，不是最终前端。
// 最终前端仍在 docs/adr/0001 里规定的 React + TypeScript + Vite 栈上。
// 之所以先用无构建版本：本机磁盘紧张，不做 node_modules 安装与前端打包。
package webui

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed index.html app.js style.css
var assets embed.FS

// Index 返回入口页面。非 /api 路径都落到这里（SPA 行为）。
func Index(w http.ResponseWriter, r *http.Request) {
	serve(w, "index.html", "text/html; charset=utf-8")
}

// Asset 返回静态资源。
func Asset(name string) http.HandlerFunc {
	ctype := "text/plain; charset=utf-8"
	switch strings.ToLower(name[strings.LastIndex(name, ".")+1:]) {
	case "js":
		ctype = "text/javascript; charset=utf-8"
	case "css":
		ctype = "text/css; charset=utf-8"
	case "html":
		ctype = "text/html; charset=utf-8"
	}
	return func(w http.ResponseWriter, r *http.Request) { serve(w, name, ctype) }
}

func serve(w http.ResponseWriter, name, ctype string) {
	b, err := assets.ReadFile(name)
	if err != nil {
		http.Error(w, "资源缺失: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ctype)
	// 界面随二进制走，禁用缓存以免升级后仍拿到旧脚本。
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}
