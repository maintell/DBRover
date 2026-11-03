package api

import (
	"net/http"
	"testing"

	"github.com/maintell/DBRover/internal/policy"
	"github.com/maintell/DBRover/internal/service"
)

// TestClassifyBlock 锁住「拦截原因 → 错误码」的映射。
//
// 两类拦截的语义完全不同：策略类（只读 / 待审批）是 403/428，客户端该去
// 换连接或补审批；语句本身在这个引擎上没法执行的是 400，补审批也还是失败。
// 混在一起的后果是客户端反复重试一个永远不会过的请求。
func TestClassifyBlock(t *testing.T) {
	cases := []struct {
		name   string
		out    service.Outcome
		code   string
		status int
	}{
		{
			name:   "具体预检给的码优先",
			out:    service.Outcome{Blocked: true, BlockCode: "CROSS_DATABASE_UNSUPPORTED"},
			code:   "CROSS_DATABASE_UNSUPPORTED",
			status: http.StatusBadRequest,
		},
		{
			name:   "只读连接仍是 403",
			out:    service.Outcome{Blocked: true, Friction: policy.F5},
			code:   "POLICY_READ_ONLY",
			status: http.StatusForbidden,
		},
		{
			name:   "需要审批仍是 428",
			out:    service.Outcome{Blocked: true, Friction: policy.F3},
			code:   "POLICY_APPROVAL_REQUIRED",
			status: http.StatusPreconditionRequired,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, status, _ := classifyBlock(&c.out)
			if code != c.code || status != c.status {
				t.Fatalf("得到 %s/%d，期望 %s/%d", code, status, c.code, c.status)
			}
		})
	}
}
