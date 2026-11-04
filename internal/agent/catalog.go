package agent

import "sort"

// Catalog 列出全部工具的对外契约（名称、说明、参数 schema），不含处理函数。
//
// buildTools 不依赖连接与模型——它只组装闭包，real 依赖在调用时才解引用，
// 所以没有连接的进程也能列出真实清单（gate 测试就是这么用的）。这样
// 「这个 Agent 到底能做什么」就有 HTTP 出口：界面、脚本、验收都能查，
// 而不是只写在系统提示里。
//
// 顺序按名字排序：map 迭代顺序随机，直接返回会让 /agent/tools 每次响应都不同，
// 快照比对与「前后是否一致」这类检查根本做不了。
func Catalog() []ToolSchema {
	tools := (&Runtime{}).buildTools()
	out := make([]ToolSchema, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.schema())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
