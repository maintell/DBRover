package service_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/policy"
	"github.com/maintell/DBRover/internal/service"
)

func sampleEvent(id string, risk policy.Level) service.AuditEvent {
	return service.AuditEvent{
		ID: id, At: time.Unix(0, 0).UTC(), Actor: "human:test",
		Action: "query.execute", Risk: risk, Friction: policy.F0,
		Outcome: "ok", SQL: "SELECT 1", DurationMS: 3,
	}
}

func TestAuditLogWritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit", "events.jsonl")

	log, err := service.NewAuditLog(path, 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	log.Record(sampleEvent("a1", policy.R0))
	log.Record(sampleEvent("a2", policy.R3))
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开审计文件: %v", err)
	}
	defer func() { _ = f.Close() }()

	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v", len(lines)+1, err)
		}
		lines = append(lines, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读取: %v", err)
	}

	if len(lines) != 2 {
		t.Fatalf("行数 = %d, 期望 2", len(lines))
	}
	if lines[0]["id"] != "a1" || lines[1]["id"] != "a2" {
		t.Errorf("顺序不符: %v", lines)
	}
	// 等级必须以 "R0"/"R3" 形式落盘，而不是数字。
	if lines[0]["risk"] != "R0" {
		t.Errorf("risk = %v, 期望 R0", lines[0]["risk"])
	}
	if lines[1]["risk"] != "R3" {
		t.Errorf("risk = %v, 期望 R3", lines[1]["risk"])
	}

	// 文件权限必须是 0600：审计里含 SQL 文本。
	// Windows 不遵守 POSIX 权限位（用 ACL），Go 的 os 包也不设 ACL，
	// 因此在 Windows 上跳过；默认数据目录位于用户 profile 下，本身就只对该用户开放。
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if mode := st.Mode().Perm(); mode != 0o600 {
			t.Errorf("文件权限 = %o, 期望 600", mode)
		}
	}
}

func TestAuditLogAppendsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")

	l1, err := service.NewAuditLog(path, 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l1.Record(sampleEvent("first", policy.R0))
	_ = l1.Close()

	l2, err := service.NewAuditLog(path, 0)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	l2.Record(sampleEvent("second", policy.R1))
	_ = l2.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if n := countLines(b); n != 2 {
		t.Errorf("行数 = %d, 期望 2（追加而非覆盖）", n)
	}
}

func TestAuditLogRecentTrims(t *testing.T) {
	log, err := service.NewAuditLog("", 3)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	for i := 0; i < 5; i++ {
		log.Record(sampleEvent(string(rune('a'+i)), policy.R0))
	}

	recent := log.Recent()
	if len(recent) != 3 {
		t.Fatalf("内存热区 = %d 条, 期望 3", len(recent))
	}
	if recent[0].ID != "c" || recent[2].ID != "e" {
		t.Errorf("应保留最新 3 条, 实际首尾 %s/%s", recent[0].ID, recent[2].ID)
	}
}

func TestAuditLogRecentIsCopy(t *testing.T) {
	log, _ := service.NewAuditLog("", 10)
	log.Record(sampleEvent("x", policy.R0))

	got := log.Recent()
	got[0].ID = "被外部改掉"
	if log.Recent()[0].ID != "x" {
		t.Error("Recent 应返回副本，外部修改不得影响内部状态")
	}
}

func TestAuditLogEmptyPathIsMemoryOnly(t *testing.T) {
	log, err := service.NewAuditLog("", 0)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	if log.Path() != "" {
		t.Errorf("Path = %q, 期望空", log.Path())
	}
	log.Record(sampleEvent("m", policy.R0))
	if len(log.Recent()) != 1 {
		t.Error("纯内存模式也应保留事件")
	}
	if err := log.Close(); err != nil {
		t.Errorf("Close 不应报错: %v", err)
	}
}

func TestSliceAuditImplementsReader(t *testing.T) {
	var s service.SliceAudit
	s.Record(sampleEvent("s1", policy.R0))
	var r service.AuditReader = &s
	if len(r.Recent()) != 1 {
		t.Error("SliceAudit 应可读")
	}
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}
