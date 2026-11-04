package service

import (
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/model"
)

// TestSQLLiteral 逐类验证字面量写法：这些差异看着小，写错就是语法错误。
func TestSQLLiteral(t *testing.T) {
	ts := time.Date(2026, 11, 4, 12, 30, 15, 0, time.UTC)
	cases := []struct {
		name   string
		engine model.Engine
		in     any
		want   string
	}{
		{"NULL", model.EngineMySQL, nil, "NULL"},
		{"单引号双写", model.EnginePostgres, "it's", "'it''s'"},
		{"二进制 mysql", model.EngineMySQL, []byte{0x01, 0xff}, "X'01ff'"},
		{"二进制 sqlite", model.EngineSQLite, []byte{0x01, 0xff}, "X'01ff'"},
		{"二进制 pg", model.EnginePostgres, []byte{0x01, 0xff}, "'\\x01ff'::bytea"},
		{"布尔 pg", model.EnginePostgres, true, "TRUE"},
		{"布尔 mysql", model.EngineMySQL, true, "1"},
		{"布尔 mysql false", model.EngineMySQL, false, "0"},
		{"时间", model.EngineMySQL, ts, "'2026-11-04 12:30:15'"},
		{"整数", model.EnginePostgres, int64(42), "42"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sqlLiteral(c.engine, c.in); got != c.want {
				t.Errorf("sqlLiteral(%v) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}
