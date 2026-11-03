package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/service"
)

// TestDDLAlterColumnTypeUnsupportedEngine 验证 SQLite 上的拒绝路径：
// 它只能重建表，所以必须在生成预案时就明确拒绝，而不是给一条必然失败的语句。
func TestDDLAlterColumnTypeUnsupportedEngine(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	_, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table:  tableRef("t"),
		Action: service.DDLAlterColumnType,
		Column: service.ColumnSpec{Name: "v", Type: "VARCHAR(4)"},
	})
	if !errors.Is(err, service.ErrInvalidDDL) {
		t.Fatalf("SQLite 上改列类型应返回 ErrInvalidDDL, 实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不支持") {
		t.Errorf("错误信息应说明不支持改列类型, 实际: %v", err)
	}
}

// TestDDLAlterColumnTypeRejectsBadInput 覆盖参数校验（与引擎能力无关的部分）。
func TestDDLAlterColumnTypeRejectsBadInput(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	cases := []struct {
		name string
		req  service.DDLRequest
	}{
		{"列不存在", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLAlterColumnType,
			Column: service.ColumnSpec{Name: "nope", Type: "VARCHAR(4)"},
		}},
		{"类型带注入", service.DDLRequest{
			Table: tableRef("t"), Action: service.DDLAlterColumnType,
			Column: service.ColumnSpec{Name: "v", Type: "TEXT; DROP TABLE t"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ex.BuildDDLPlan(ctx, c.req); !errors.Is(err, service.ErrInvalidDDL) {
				t.Fatalf("应返回 ErrInvalidDDL, 实际 %v", err)
			}
		})
	}
}
