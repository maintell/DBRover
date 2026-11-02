package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/maintell/DBRover/internal/service"
)

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
		wantCat  string
		status   int
		retry    bool
	}{
		{"取消", context.Canceled, "SQL_CANCELED", "canceled", 499, false},
		{"超时", context.DeadlineExceeded, "SQL_TIMEOUT", "timeout", 504, true},
		{"表不存在", errors.New("SQL logic error: no such table: t (1)"), "SQL_OBJECT_NOT_FOUND", "syntax", 400, false},
		{"列不存在", errors.New("no such column: nope"), "SQL_OBJECT_NOT_FOUND", "syntax", 400, false},
		{"语法错", errors.New(`near "FORM": syntax error`), "SQL_SYNTAX_ERROR", "syntax", 400, false},
		{"缺参数", errors.New("ERROR: there is no parameter $1 (SQLSTATE 42P02)"), "SQL_PARAMETER_MISSING", "syntax", 400, false},
		{"唯一约束", errors.New("UNIQUE constraint failed: t.id"), "SQL_CONSTRAINT_VIOLATION", "constraint", 409, false},
		{"外键", errors.New("FOREIGN KEY constraint failed"), "SQL_CONSTRAINT_VIOLATION", "constraint", 409, false},
		{"只读库", errors.New("attempt to write a readonly database"), "POLICY_READ_ONLY", "policy", 403, false},
		{"锁", errors.New("database is locked (5)"), "DB_LOCKED", "resource", 503, true},
		{"权限", errors.New("access denied for user"), "DB_PERMISSION_DENIED", "permission", 403, false},
		{"未知", errors.New("something odd"), "SQL_EXEC_FAILED", "internal", 500, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := service.ClassifyError(c.err)
			if got == nil {
				t.Fatal("应返回非 nil")
			}
			if got.Code != c.wantCode {
				t.Errorf("Code = %s, 期望 %s", got.Code, c.wantCode)
			}
			if got.Category != c.wantCat {
				t.Errorf("Category = %s, 期望 %s", got.Category, c.wantCat)
			}
			if s := got.HTTPStatus(); s != c.status {
				t.Errorf("HTTPStatus = %d, 期望 %d", s, c.status)
			}
			if got.Retryable != c.retry {
				t.Errorf("Retryable = %v, 期望 %v", got.Retryable, c.retry)
			}
		})
	}

	if service.ClassifyError(nil) != nil {
		t.Error("nil 应返回 nil")
	}
}
