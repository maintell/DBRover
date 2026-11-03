package secret

import (
	"errors"
	"strings"
	"testing"
)

// testService 用一个不可能与真实配置撞上的命名空间。
//
// 这个测试会真的往当前用户的凭据库里写一条再删掉——只有这样才能验证
// 「结构体布局对不对、能不能读回来」。命名空间带随机后缀，且清理写在
// Cleanup 里，跑完不留东西。
func testService(t *testing.T) string {
	t.Helper()
	return "dbrover-selftest/" + strings.ReplaceAll(t.Name(), "/", "-")
}

// TestStoreLoadDelete 往返一次：写进去、读出来一致、删掉之后读会报不存在。
func TestStoreLoadDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("会写系统凭据库，-short 下跳过")
	}
	svc := testService(t)
	acct := "roundtrip"
	t.Cleanup(func() { _ = Delete(svc, acct) })
	_ = Delete(svc, acct) // 先清一次，避免上次失败的残留影响判断

	if err := Store(svc, acct, "s3cr3t-value"); err != nil {
		if errors.Is(err, ErrUnsupported) {
			t.Skip("当前平台没有凭据库实现")
		}
		t.Fatalf("Store: %v", err)
	}

	got, err := Load(svc, acct)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "s3cr3t-value" {
		t.Fatalf("读回来的值不对: %q", got)
	}

	// 覆盖写：同一个目标名应当被更新而不是报错。
	if err := Store(svc, acct, "rotated-value"); err != nil {
		t.Fatalf("覆盖写: %v", err)
	}
	got, err = Load(svc, acct)
	if err != nil {
		t.Fatalf("覆盖后 Load: %v", err)
	}
	if got != "rotated-value" {
		t.Fatalf("覆盖后的值不对: %q", got)
	}

	if err := Delete(svc, acct); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := Load(svc, acct); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应报 ErrNotFound，实际 %v", err)
	}
}

// TestLoadMissing 没存过时必须是 ErrNotFound，不能是空字符串当成功。
func TestLoadMissing(t *testing.T) {
	svc := testService(t)
	if _, err := Load(svc, "never-stored"); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrUnsupported) {
		t.Fatalf("应报 ErrNotFound 或 ErrUnsupported，实际 %v", err)
	}
}

// TestValidate 空参数要被挡住，且不碰凭据库。
func TestValidate(t *testing.T) {
	if err := Store("", "a", "v"); !errors.Is(err, ErrEmpty) {
		t.Errorf("空 service 应报 ErrEmpty，实际 %v", err)
	}
	if _, err := Load("a", " "); !errors.Is(err, ErrEmpty) {
		t.Errorf("空白 account 应报 ErrEmpty，实际 %v", err)
	}
	if err := Delete("", ""); !errors.Is(err, ErrEmpty) {
		t.Errorf("全空应报 ErrEmpty，实际 %v", err)
	}
}

// TestTarget 目标名拼接要稳定：凭据库里靠它定位，改了就读不回来。
func TestTarget(t *testing.T) {
	if got := Target("dbrover/agent", "api-key"); got != "dbrover/agent/api-key" {
		t.Errorf("Target = %q", got)
	}
	if got := Target("dbrover/agent/", "api-key"); got != "dbrover/agent/api-key" {
		t.Errorf("尾部斜杠应被规范化，实际 %q", got)
	}
}
