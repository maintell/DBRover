package service

import (
	"context"
	"errors"
	"strings"
)

// ExecError 是执行期错误的结构化表示，对应 docs/12 §4.2 的错误码表。
//
// 为什么需要它：驱动错误是自由文本，界面和 Agent 都只能猜。
// 客户端按 code 分支（文档明确"不要匹配 message"），必须有人把它归类。
type ExecError struct {
	Code      string
	Category  string
	Message   string
	Hint      string
	Retryable bool
}

func (e *ExecError) Error() string { return e.Message }

// ClassifyError 把驱动/上下文错误映射为稳定错误码。
//
// 分类靠错误文本匹配，是各引擎驱动不给结构化错误码时的现实选择。
// 匹配不到时落到 SQL_EXEC_FAILED（不可重试），宁可让用户看到原文，
// 也不要假装知道原因。
func ClassifyError(err error) *ExecError {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, context.Canceled):
		return &ExecError{
			Code: "SQL_CANCELED", Category: "canceled",
			Message: "执行已取消", Retryable: false,
		}
	case errors.Is(err, context.DeadlineExceeded):
		return &ExecError{
			Code: "SQL_TIMEOUT", Category: "timeout",
			Message: "执行超时", Hint: "缩小查询范围或提高超时时间", Retryable: true,
		}
	}

	msg := err.Error()
	low := strings.ToLower(msg)

	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(low, s) {
				return true
			}
		}
		return false
	}

	switch {
	case has("readonly database", "read-only", "attempt to write a readonly", "query_only"):
		return &ExecError{
			Code: "POLICY_READ_ONLY", Category: "policy",
			Message: msg, Hint: "该连接是只读的；如需写入请新建读写连接", Retryable: false,
		}
	case has("no such table", "no such column", "no such function", "unknown column", "unknown table"):
		return &ExecError{
			Code: "SQL_OBJECT_NOT_FOUND", Category: "syntax",
			Message: msg, Hint: "检查对象名与当前库/模式是否正确", Retryable: false,
		}
	case has("there is no parameter", "missing parameter", "parameter not set"):
		return &ExecError{
			Code: "SQL_PARAMETER_MISSING", Category: "syntax",
			Message: msg, Hint: "SQL 里有占位符但没有提供参数；检查参数个数与顺序", Retryable: false,
		}
	case has("syntax error", "near \"", "unrecognized token", "incomplete input"):
		return &ExecError{
			Code: "SQL_SYNTAX_ERROR", Category: "syntax",
			Message: msg, Hint: "检查语法与引号闭合", Retryable: false,
		}
	case has("unique constraint failed", "foreign key constraint failed",
		"not null constraint failed", "check constraint failed", "constraint failed"):
		return &ExecError{
			Code: "SQL_CONSTRAINT_VIOLATION", Category: "constraint",
			Message: msg, Hint: "违反唯一/外键/非空/检查约束，先处理冲突数据", Retryable: false,
		}
	case has("database is locked", "database table is locked", "busy"):
		return &ExecError{
			Code: "DB_LOCKED", Category: "resource",
			Message: msg, Hint: "有其它写事务占用；稍后重试或缩短事务", Retryable: true,
		}
	case has("permission denied", "not authorized", "access denied"):
		return &ExecError{
			Code: "DB_PERMISSION_DENIED", Category: "permission",
			Message: msg, Retryable: false,
		}
	}

	return &ExecError{
		Code: "SQL_EXEC_FAILED", Category: "internal",
		Message: msg, Retryable: false,
	}
}

// HTTPStatus 把错误分类映射为 HTTP 状态码（docs/12 §4.2）。
func (e *ExecError) HTTPStatus() int {
	switch e.Category {
	case "syntax":
		return 400
	case "constraint":
		return 409
	case "permission", "policy":
		return 403
	case "timeout":
		return 504
	case "canceled":
		return 499
	case "resource":
		return 503
	case "connection":
		return 502
	}
	return 500
}
