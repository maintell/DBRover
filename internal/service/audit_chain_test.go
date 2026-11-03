package service_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/policy"
	"github.com/maintell/DBRover/internal/service"
)

// ev 造一条审计事件。
func ev(id string) service.AuditEvent {
	return service.AuditEvent{
		ID: id, Actor: "human:local", Action: "query.execute",
		Target: "t", Risk: policy.R1, Friction: policy.F2,
		Outcome: "ok", SQL: "UPDATE t SET v = 1", DurationMS: 3,
	}
}

// TestAuditChainVerifyOK 正常写入后链条应当是完整的。
func TestAuditChainVerifyOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := service.NewAuditLog(path, 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		l.Record(ev(id))
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 重新打开再校验：链必须能跨进程接上。
	l2, err := service.NewAuditLog(path, 0)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer func() { _ = l2.Close() }()

	res, err := l2.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK || res.Events != 3 {
		t.Fatalf("链条应完整且有 3 条，实际 %+v", res)
	}
	if res.BrokenAt != 0 {
		t.Errorf("不该有断点: %+v", res)
	}
}

// TestAuditChainDetectsTamper 对应 docs/10 验收 9：
// 手工改中间一行，校验必须指到那一行。
func TestAuditChainDetectsTamper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := service.NewAuditLog(path, 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		l.Record(ev(id))
	}
	_ = l.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("应有 3 行，实际 %d", len(lines))
	}
	// 只把第二行的 outcome 改掉——这正是「悄悄改一条」的手法。
	tampered := strings.Replace(lines[1], `"outcome":"ok"`, `"outcome":"denied"`, 1)
	if tampered == lines[1] {
		t.Fatalf("没改到内容，第二行: %s", lines[1])
	}
	lines[1] = tampered
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("写回审计: %v", err)
	}

	l2, err := service.NewAuditLog(path, 0)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer func() { _ = l2.Close() }()

	res, err := l2.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("被改过却报完整——链校验没起作用")
	}
	if res.BrokenAt != 2 {
		t.Errorf("断点应指向第 2 行，实际 %d（%s）", res.BrokenAt, res.Reason)
	}
}

// TestAuditChainDetectsDeletedLine 删掉中间一条也要能发现。
func TestAuditChainDetectsDeletedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := service.NewAuditLog(path, 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		l.Record(ev(id))
	}
	_ = l.Close()

	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	// 删掉中间那条：剩下 1、3 两行。
	if err := os.WriteFile(path,
		[]byte(lines[0]+"\n"+lines[2]+"\n"), 0o600); err != nil {
		t.Fatalf("写回审计: %v", err)
	}

	l2, err := service.NewAuditLog(path, 0)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer func() { _ = l2.Close() }()

	res, err := l2.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("删掉一条却报完整——链校验没起作用")
	}
	if res.BrokenAt != 2 {
		t.Errorf("断点应指向第 2 行（接不上），实际 %d（%s）", res.BrokenAt, res.Reason)
	}
}

// TestAuditChainContinuesAfterRestart 重启后写的事件要继续挂在上一条后面，
// 否则每次重启都会开一条新链、校验必然报断点。
func TestAuditChainContinuesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := service.NewAuditLog(path, 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l.Record(ev("a"))
	l.Record(ev("b"))
	_ = l.Close()

	l2, err := service.NewAuditLog(path, 0)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	l2.Record(ev("c"))
	if err := l2.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	l3, err := service.NewAuditLog(path, 0)
	if err != nil {
		t.Fatalf("再次打开: %v", err)
	}
	defer func() { _ = l3.Close() }()
	res, err := l3.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK || res.Events != 3 {
		t.Fatalf("跨重启的链条应完整且有 3 条，实际 %+v", res)
	}
}

// TestAuditChainInMemory 未落盘时明确说「没有可校验的链条」，不假装通过。
func TestAuditChainInMemory(t *testing.T) {
	l, err := service.NewAuditLog("", 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	defer func() { _ = l.Close() }()
	l.Record(ev("a"))

	res, err := l.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK || res.Reason == "" {
		t.Fatalf("内存模式应说明没有链条可校验: %+v", res)
	}
}
