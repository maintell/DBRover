package api

import (
	"net/http"
	"testing"

	"github.com/maintell/DBRover/internal/service"
)

// TestClassifyBlockPlanExpiredIs404 锁住「过期预案不是「需要审批」」。
//
// 这个码以前根本没有：过期走的是 Blocked + 无 BlockCode 的路径，被归成
// POLICY_APPROVAL_REQUIRED/428，hint 还写着「用 approve=true 重新提交」——
// 客户端于是对一张永远不可能成功的预案反复重试。过期是不可逆状态，正确动作
// 只有重新生成，所以给 404。
func TestClassifyBlockPlanExpiredIs404(t *testing.T) {
	code, status, hint := classifyBlock(&service.Outcome{
		Blocked:   true,
		BlockCode: "CHANGE_PLAN_EXPIRED",
		BlockHint: "重新生成预案",
		Reason:    "变更预案已过期，请重新生成",
	})
	if code != "CHANGE_PLAN_EXPIRED" {
		t.Errorf("错误码 = %q", code)
	}
	if status != http.StatusNotFound {
		t.Errorf("状态码 = %d，期望 404（不是 428：补审批也救不回来）", status)
	}
	if hint == "" {
		t.Error("必须给可行动的 hint")
	}
}

// TestClassifyBlockOtherCodesStay400 反向断言：别的拦截码仍然是 400。
//
// 少了这条，把 switch 写成「所有 BlockCode 都 404」也会通过——那会把跨库引用
// 这类「改写法就能过」的拦截说成「不存在」。
func TestClassifyBlockOtherCodesStay400(t *testing.T) {
	code, status, _ := classifyBlock(&service.Outcome{
		Blocked:   true,
		BlockCode: "CROSS_DATABASE_UNSUPPORTED",
		Reason:    "不支持跨库引用",
	})
	if code != "CROSS_DATABASE_UNSUPPORTED" || status != http.StatusBadRequest {
		t.Fatalf("(code,status) = (%q,%d)，期望 (CROSS_DATABASE_UNSUPPORTED,400)", code, status)
	}
}
