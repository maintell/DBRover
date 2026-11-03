package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAIConfig 是 OpenAI 兼容服务的连接参数。
//
// 「兼容」这个词要落到实处：DeepSeek、通义、Moonshot、vLLM、以及 Ollama 的
// /v1 端点都实现同一套 chat/completions 协议。所以这里不按厂商分支，
// 只认协议——厂商差异由 base_url 与 model 名消化。
type OpenAIConfig struct {
	// BaseURL 形如 https://api.openai.com/v1（末尾不要带斜杠）。
	BaseURL string
	APIKey  string
	Model   string
	// HTTPClient 为空时用带超时的默认客户端。
	HTTPClient *http.Client
	// Timeout 为单次补全的上限，默认 120 秒。
	Timeout time.Duration
}

// OpenAI 是 OpenAI 兼容协议的客户端。
type OpenAI struct {
	cfg    OpenAIConfig
	client *http.Client
}

// NewOpenAI 构造客户端。
func NewOpenAI(cfg OpenAIConfig) (*OpenAI, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("agent: 缺少 base_url")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("agent: 缺少 model")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &OpenAI{cfg: cfg, client: client}, nil
}

// Name 返回给界面显示的服务名。
func (o *OpenAI) Name() string { return "openai-compatible:" + o.cfg.Model }

// Caps 声明能力。这一层只有工具调用一种花样，流式留给后续。
func (o *OpenAI) Caps() ProviderCaps { return ProviderCaps{ToolCalling: true} }

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content,omitempty"`
	Name       string       `json:"name,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Function oaFunction `json:"function"`
}

type oaFunction struct {
	Name string `json:"name"`
	// Arguments 在协议里是**一个 JSON 字符串**，不是对象。
	Arguments string `json:"arguments"`
}

type oaTool struct {
	Type     string     `json:"type"`
	Function ToolSchema `json:"function"`
}

type oaRequest struct {
	Model    string      `json:"model"`
	Messages []oaMessage `json:"messages"`
	Tools    []oaTool    `json:"tools,omitempty"`
	Stream   bool        `json:"stream"`
}

type oaResponse struct {
	Choices []struct {
		Message      oaMessage `json:"message"`
		FinishReason string    `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete 发一次补全请求。
func (o *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	body := oaRequest{Model: o.cfg.Model, Stream: false}
	// system 单独放在最前面：有些网关只认第一条 system，混在 messages 里会被忽略。
	if strings.TrimSpace(req.System) != "" {
		body.Messages = append(body.Messages, oaMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		om := oaMessage{Role: m.Role, Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID}
		for _, c := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, oaToolCall{
				ID: c.ID, Type: "function",
				Function: oaFunction{Name: c.Name, Arguments: string(c.Arguments)},
			})
		}
		body.Messages = append(body.Messages, om)
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, oaTool{Type: "function", Function: t})
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.cfg.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	// 只读前 64KB：错误页有时是整页 HTML，全读进来既没用又费内存。
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("agent: 模型服务返回 %d：%s",
			resp.StatusCode, snippet(payload))
	}

	var parsed oaResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return Response{}, fmt.Errorf("agent: 响应不是合法 JSON：%w（原文：%s）", err, snippet(payload))
	}
	if parsed.Error != nil {
		return Response{}, fmt.Errorf("agent: 模型服务报错：%s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, fmt.Errorf("agent: 响应里没有 choices：%s", snippet(payload))
	}

	msg := parsed.Choices[0].Message
	out := Response{Text: msg.Content, Usage: parsed.Usage}
	for _, c := range msg.ToolCalls {
		args := strings.TrimSpace(c.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			// 模型偶尔会给出不合法 JSON。这里不猜、不修，原样带给它看，
			// 让它自己按 schema 重来一次。
			args = `{"__invalid_arguments__":` + quoteForJSON(c.Function.Arguments) + `}`
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID: c.ID, Name: c.Function.Name, Arguments: json.RawMessage(args),
		})
	}
	return out, nil
}

// snippet 取响应的开头一段，用于错误信息。日志里不能出现整页 HTML。
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

func quoteForJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
