package service_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/service"
)

// newAnchor 建一个带临时密钥的锚点。
func newAnchor(t *testing.T, dir string, key string) *service.Anchor {
	t.Helper()
	a, err := service.OpenAnchor(filepath.Join(dir, "audit.anchor"), []byte(key))
	if err != nil {
		t.Fatalf("OpenAnchor: %v", err)
	}
	return a
}

// TestAnchorSealAndVerify 正常路径：封存过的链条，校验时报「锚点一致」。
func TestAnchorSealAndVerify(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")

	l, err := service.NewAuditLog(auditPath, 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l.SetAnchor(newAnchor(t, dir, "test-key"))
	for _, id := range []string{"a", "b", "c"} {
		l.Record(ev(id))
	}
	if err := l.Close(); err != nil { // Close 会封存最后一次
		t.Fatalf("Close: %v", err)
	}

	l2, err := service.NewAuditLog(auditPath, 0)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer func() { _ = l2.Close() }()
	l2.SetAnchor(newAnchor(t, dir, "test-key"))

	res, err := l2.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("链条应完整: %+v", res)
	}
	if !res.AnchorChecked || !res.AnchorOK {
		t.Fatalf("应报告锚点一致: %+v", res)
	}
}

// TestAnchorDetectsFullRewrite 是这套机制存在的理由：
// 有人把审计文件整段重写成一条**自洽**的新链——链校验过得去，锚点过不去。
func TestAnchorDetectsFullRewrite(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")

	original, err := service.NewAuditLog(auditPath, 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	original.SetAnchor(newAnchor(t, dir, "test-key"))
	for _, id := range []string{"a", "b", "c"} {
		original.Record(ev(id))
	}
	if err := original.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 伪造者：在别处生成一条完全自洽的新链（内容不同、条数相同）。
	fakePath := filepath.Join(dir, "fake.jsonl")
	fake, err := service.NewAuditLog(fakePath, 0)
	if err != nil {
		t.Fatalf("建伪造链: %v", err)
	}
	for _, id := range []string{"x", "y", "z"} {
		fake.Record(ev(id))
	}
	_ = fake.Close()

	body, err := os.ReadFile(fakePath)
	if err != nil {
		t.Fatalf("读伪造链: %v", err)
	}
	if err := os.WriteFile(auditPath, body, 0o600); err != nil {
		t.Fatalf("覆盖审计文件: %v", err)
	}

	l, err := service.NewAuditLog(auditPath, 0)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer func() { _ = l.Close() }()
	l.SetAnchor(newAnchor(t, dir, "test-key"))

	res, err := l.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("整段重写却报完整——锚点没起作用")
	}
	if !res.AnchorChecked || res.AnchorOK {
		t.Fatalf("应明确报锚点不一致: %+v", res)
	}
	if !strings.Contains(res.Reason, "与锚点不一致") {
		t.Errorf("原因应说明是锚点不一致: %s", res.Reason)
	}
}

// TestAnchorDetectsTruncation 只删掉尾部几条（链仍然自洽），锚点要能发现条数不对。
func TestAnchorDetectsTruncation(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")

	l, err := service.NewAuditLog(auditPath, 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l.SetAnchor(newAnchor(t, dir, "test-key"))
	for _, id := range []string{"a", "b", "c", "d"} {
		l.Record(ev(id))
	}
	_ = l.Close()

	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("应有 4 行，实际 %d", len(lines))
	}
	// 砍掉最后一行：剩下 3 条仍然是一条自洽的链。
	if err := os.WriteFile(auditPath, []byte(strings.Join(lines[:3], "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("写回: %v", err)
	}

	l2, err := service.NewAuditLog(auditPath, 0)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer func() { _ = l2.Close() }()
	l2.SetAnchor(newAnchor(t, dir, "test-key"))

	res, err := l2.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("被截断却报完整——锚点没起作用")
	}
	if !strings.Contains(res.Reason, "与锚点不一致") {
		t.Errorf("原因应说明锚点不一致: %s", res.Reason)
	}
}

// TestAnchorRejectsWrongKey 密钥不对时，锚点自己就该报错，不能当成「没问题」。
func TestAnchorRejectsWrongKey(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")

	l, err := service.NewAuditLog(auditPath, 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l.SetAnchor(newAnchor(t, dir, "right-key"))
	l.Record(ev("a"))
	_ = l.Close()

	l2, err := service.NewAuditLog(auditPath, 0)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer func() { _ = l2.Close() }()
	l2.SetAnchor(newAnchor(t, dir, "wrong-key"))

	res, err := l2.Verify()
	if err != nil {
		t.Fatalf("Verify 不该返回错误（结论在 res 里）: %v", err)
	}
	if res.OK {
		t.Fatal("密钥不对却报完整")
	}
	if !errors.Is(service.ErrAnchorMAC, service.ErrAnchorMAC) { // 仅为引用该错误值
		t.Fatal("unreachable")
	}
}

// TestAnchorRequiresKey 没有密钥的锚点等于自签收据，必须直接拒绝。
func TestAnchorRequiresKey(t *testing.T) {
	if _, err := service.OpenAnchor(filepath.Join(t.TempDir(), "a.anchor"), nil); err == nil {
		t.Fatal("缺少密钥应报错")
	}
	if _, err := service.OpenAnchor("", []byte("k")); err == nil {
		t.Fatal("缺少路径应报错")
	}
}
