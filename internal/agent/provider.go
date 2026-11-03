package agent

import (
	"context"
	"encoding/json"
)

// ToolSchema 是一个工具对外暴露的契约。
//
// Parameters 是 JSON Schema 原文：模型据此构造参数，我们据此做基础校验。
// 不做「宽松解析」——参数不对就让模型看到明确的错误，而不是猜它的意思。
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall 是模型请求执行一次工具调用。
type ToolCall struct {
	ID string `json:"id"`
	// Name 是工具名，形如 meta.objects。
	Name string `json:"name"`
	// Arguments 是原始 JSON 参数。原样保留：我们只看自己要的字段，
	// 多余字段一律拒绝（docs/11 §3.1 第 3 条）。
	Arguments json.RawMessage `json:"arguments"`
}

// Message 是一轮对话中的一条消息。
type Message struct {
	// Role 取 system / user / assistant / tool。
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// Name 是工具名，仅在 Role 为 tool 时出现。
	Name string `json:"name,omitempty"`
	// ToolCalls 仅在 Role 为 assistant 且模型要调工具时出现。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 关联到发起调用的那个 ToolCall。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// Usage 是一次补全的 token 消耗。各家的字段名一致，取值缺失时为 0。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Request 是一次补全请求。
type Request struct {
	System   string
	Messages []Message
	Tools    []ToolSchema
}

// Response 是一次补全的结果。ToolCalls 为空表示模型给出了最终答复。
type Response struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
}

// ProviderCaps 声明一个 Provider 支持哪些能力。
//
// 不支持工具调用的模型不允许进入 Agent 模式（docs/11 §8.2），
// 只能降级成单轮补全——这个判断由 Capabilities 决定，不由配置里写什么决定。
type ProviderCaps struct {
	ToolCalling bool `json:"tool_calling"`
	Streaming   bool `json:"streaming"`
	JSONMode    bool `json:"json_mode"`
}

// Provider 是一个模型服务。
//
// 刻意只有一个方法：先做「问一次、要么回答要么调工具」这一条路径，
// 流式与多轮压缩都在它之上叠加，不进来搅浑。
type Provider interface {
	Name() string
	Caps() ProviderCaps
	Complete(ctx context.Context, req Request) (Response, error)
}
