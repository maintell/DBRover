package agent

import (
	"encoding/json"
	"fmt"
)

// Envelope 是工具返回给模型的统一信封（docs/11 §3.3）。
//
// 为什么不是「直接把结果 JSON 丢回去」：模型需要知道三件不同的事——
// 这次成功了吗、看到的是全部还是截断、下一步还能做什么。这三件事混在
// 数据里，模型就会倾向于重复调用或编造。
type Envelope struct {
	OK          bool       `json:"ok"`
	Summary     string     `json:"summary,omitempty"`
	Data        any        `json:"data,omitempty"`
	Truncated   *Truncated `json:"truncated,omitempty"`
	Warnings    []string   `json:"warnings,omitempty"`
	NextActions []string   `json:"next_actions,omitempty"`
	Error       *ToolError `json:"error,omitempty"`
}

// Truncated 说明结果被截断了多少，以及怎么缩小范围。
type Truncated struct {
	RowsShown int    `json:"rows_shown"`
	RowsTotal int    `json:"rows_total"`
	Hint      string `json:"hint,omitempty"`
}

// ToolError 是给模型看的结构化错误：code 用于分支，hint 用于自我修正。
type ToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

// ok 构造一个成功信封。
func ok(summary string, data any) Envelope {
	return Envelope{OK: true, Summary: summary, Data: data}
}

// fail 构造一个失败信封。失败不是异常：模型据此修正，不该让整个循环崩掉。
func fail(code, message, hint string) Envelope {
	return Envelope{OK: false, Error: &ToolError{Code: code, Message: message, Hint: hint}}
}

// encode 把信封序列化成送给模型的一段文本。
func (e Envelope) encode() string {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Sprintf(`{"ok":false,"error":{"code":"ENCODE_FAILED","message":%q}}`, err.Error())
	}
	return string(b)
}
