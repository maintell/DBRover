package service

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	// lastHash 是链上最后一条的哈希，新事件接着它算（docs/10 §6.2）。
	lastHash string
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
	// 接到已有链条的末尾：重启之后写的事件必须继续挂在上一条后面，
	// 否则每次重启都会开一条新链，校验时必然报断点。
	if h, err := lastHashInFile(path); err == nil {
		l.lastHash = h
	}
	return l, nil
}

// lastHashInFile 读出文件里最后一条事件的哈希。
//
// 顺序读整个文件：审计是追加写的，正常情况下几 MB 就很大了；真到需要优化时
// 再改成从尾部反向读。
func lastHashInFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev AuditEvent
		if err := json.Unmarshal(line, &ev); err == nil && ev.Hash != "" {
			last = ev.Hash
		}
	}
	return last, sc.Err()
}

// Record 追加一条事件。写盘失败不会向上传播——审计失败不应阻断业务，
// 但要留下可见痕迹（写 stderr）。
func (l *AuditLog) Record(ev AuditEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 先接链再落盘：PrevHash 指向上一条，Hash 覆盖本条内容。
	ev.PrevHash = l.lastHash
	ev.Hash = hashAuditEvent(ev)
	l.lastHash = ev.Hash

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

// hashAuditEvent 计算一条事件的链式哈希（docs/10 §6.2）：
// hash = SHA256(prev_hash || canonical_json(event_without_hash))。
//
// 用结构体自身的字段顺序当「规范化 JSON」：Go 的 json.Marshal 对同一结构体
// 输出字段顺序固定，读回来再算一遍能得到同样的字节。前提是 Risk / Friction
// 的编解码对称——所以它们补了 UnmarshalJSON。
func hashAuditEvent(ev AuditEvent) string {
	ev.Hash = ""
	b, err := json.Marshal(ev)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(append([]byte(ev.PrevHash), b...))
	return hex.EncodeToString(sum[:])
}

// VerifyResult 是一次完整性校验的结果。
type VerifyResult struct {
	OK     bool `json:"ok"`
	Events int  `json:"events"`
	// BrokenAt 是第一个出问题的行号（1 基）；0 表示没有断点。
	BrokenAt int    `json:"broken_at,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Verify 校验审计文件的链式哈希。
//
// 能发现：改一条内容、删中间一条、插一条、把 hash 字段删掉。
// 不能发现：有管理员权限的人把整个文件重写成一条自洽的新链——
// 那需要外部锚点（定期把链头哈希发到别处），v1 不做（docs/10 §6.2）。
func (l *AuditLog) Verify() (VerifyResult, error) {
	l.mu.Lock()
	path := l.path
	l.mu.Unlock()

	if path == "" {
		return VerifyResult{OK: true, Reason: "审计未落盘（启动时没给 -audit），没有可校验的链条"}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return VerifyResult{}, err
	}
	defer func() { _ = f.Close() }()

	prev := ""
	line := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		line++
		var ev AuditEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return VerifyResult{Events: line - 1, BrokenAt: line,
				Reason: "第 " + itoa(line) + " 行不是合法 JSON：" + err.Error()}, nil
		}
		if ev.Hash == "" {
			return VerifyResult{Events: line - 1, BrokenAt: line,
				Reason: "第 " + itoa(line) + " 行缺少 hash（旧格式，或被删掉了）"}, nil
		}
		if got := hashAuditEvent(ev); got != ev.Hash {
			return VerifyResult{Events: line - 1, BrokenAt: line,
				Reason: "第 " + itoa(line) + " 行的哈希对不上（内容被改过）"}, nil
		}
		if ev.PrevHash != prev {
			return VerifyResult{Events: line - 1, BrokenAt: line,
				Reason: "第 " + itoa(line) + " 行与前一条接不上（中间被删或插入过）"}, nil
		}
		prev = ev.Hash
	}
	if err := sc.Err(); err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{OK: true, Events: line}, nil
}

// itoa 是本地小工具，避免为了一个整数格式化再拉一个依赖。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
