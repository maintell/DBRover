package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// AuditReader 是审计的读取侧。界面与 API 通过它拿最近的事件。
type AuditReader interface {
	Recent() []AuditEvent
}

// SliceAudit 是纯内存实现，用于测试与不需要持久化的场景。
type SliceAudit struct {
	mu     sync.Mutex
	Events []AuditEvent
}

// Record 追加一条事件。
func (s *SliceAudit) Record(ev AuditEvent) {
	s.mu.Lock()
	s.Events = append(s.Events, ev)
	s.mu.Unlock()
}

// Recent 返回全部已记录事件（按写入顺序）。
func (s *SliceAudit) Recent() []AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditEvent, len(s.Events))
	copy(out, s.Events)
	return out
}

// AuditLog 是「落盘 + 内存热区」的审计实现。
//
// 追加写 JSONL，一行一条，不可修改；同时在内存里保留最近 N 条供 API 查询，
// 避免每次翻页都去读文件。这不是防篡改审计——那需要链式哈希与外部锚点，
// 见 docs/10-connection-and-security.md §6。
type AuditLog struct {
	mu     sync.Mutex
	f      *os.File
	recent []AuditEvent
	limit  int
	path   string
}

// DefaultAuditRecent 是内存热区保留的条数。
const DefaultAuditRecent = 500

// NewAuditLog 打开（或创建）审计文件。path 为空时退化为纯内存。
func NewAuditLog(path string, recentLimit int) (*AuditLog, error) {
	if recentLimit <= 0 {
		recentLimit = DefaultAuditRecent
	}
	l := &AuditLog{limit: recentLimit, path: path}
	if path == "" {
		return l, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("创建审计目录失败: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开审计文件失败: %w", err)
	}
	// 说明：0o600 只在类 Unix 上生效；Windows 走 ACL，Go 不设 ACL，
	// 实际保护来自「文件位于用户 profile 目录下」这一默认 ACL。
	l.f = f
	return l, nil
}

// Record 追加一条事件。写盘失败不会向上传播——审计失败不应阻断业务，
// 但要留下可见痕迹（写 stderr）。
func (l *AuditLog) Record(ev AuditEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.recent = append(l.recent, ev)
	if len(l.recent) > l.limit {
		drop := len(l.recent) - l.limit
		l.recent = append(l.recent[:0], l.recent[drop:]...)
	}

	if l.f == nil {
		return
	}
	b, err := json.Marshal(ev)
	if err != nil {
		fmt.Fprintf(os.Stderr, "审计序列化失败: %v\n", err)
		return
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "审计写入失败: %v\n", err)
	}
}

// Recent 返回内存热区里的事件。
func (l *AuditLog) Recent() []AuditEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]AuditEvent, len(l.recent))
	copy(out, l.recent)
	return out
}

// Path 返回审计文件路径，未落盘时为空。
func (l *AuditLog) Path() string { return l.path }

// Close 关闭文件。
func (l *AuditLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
