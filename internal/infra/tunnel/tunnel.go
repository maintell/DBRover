// Package tunnel 实现 SSH 隧道与跳板链。
//
// 只依赖 golang.org/x/crypto/ssh，不调用外部 ssh 二进制：
// 跨平台行为一致，也不要求用户机器安装 OpenSSH 客户端。
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// MaxHops 是跳板链长度上限。超过通常意味着应该改用堡垒机而不是继续串联。
const MaxHops = 4

// AuthKind 是跳板的认证方式。
type AuthKind string

const (
	AuthPassword   AuthKind = "password"
	AuthPrivateKey AuthKind = "private_key"
	AuthAgent      AuthKind = "agent"
)

// Hop 是一跳跳板配置。密码/私钥口令只在内存中存在，不落盘。
type Hop struct {
	Addr        string
	User        string
	AuthKind    AuthKind
	Password    []byte
	PrivateKey  []byte
	Passphrase  []byte
	AgentSocket string
}

// HostKeyStore 负责主机密钥校验。实现必须是「首次信任并记录，之后必须匹配」。
type HostKeyStore interface {
	Verify(addr string, key ssh.PublicKey) error
}

// Config 是隧道配置。
type Config struct {
	Hops []Hop
	// HostKeys 必填。为 nil 时 Dial 直接报错，避免出现「静默不校验」的路径。
	HostKeys HostKeyStore
	Timeout  time.Duration
}

// Tunnel 是一条已建立的隧道。关闭顺序与建立顺序相反。
type Tunnel struct {
	conn    net.Conn
	clients []*ssh.Client
}

// Conn 返回指向目标的连接。
func (t *Tunnel) Conn() net.Conn { return t.conn }

// Hops 返回实际建立的跳数。
func (t *Tunnel) Hops() int { return len(t.clients) }

// Close 关闭目标连接与所有跳板连接。
func (t *Tunnel) Close() error {
	var firstErr error
	if t.conn != nil {
		if err := t.conn.Close(); err != nil {
			firstErr = err
		}
		t.conn = nil
	}
	for i := len(t.clients) - 1; i >= 0; i-- {
		_ = t.clients[i].Close()
	}
	t.clients = nil
	return firstErr
}

// Dial 依次串联各跳板，返回指向 target（host:port）的连接。
func Dial(ctx context.Context, cfg Config, target string) (*Tunnel, error) {
	if len(cfg.Hops) == 0 {
		return nil, errors.New("隧道至少需要一跳")
	}
	if len(cfg.Hops) > MaxHops {
		return nil, fmt.Errorf("跳数 %d 超过上限 %d", len(cfg.Hops), MaxHops)
	}
	if cfg.HostKeys == nil {
		return nil, errors.New("缺少主机密钥校验实现，拒绝在无校验的情况下建立隧道")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	var clients []*ssh.Client
	closeAll := func() {
		for i := len(clients) - 1; i >= 0; i-- {
			_ = clients[i].Close()
		}
	}

	for _, hop := range cfg.Hops {
		auths, err := authMethods(hop)
		if err != nil {
			closeAll()
			return nil, err
		}

		var raw net.Conn
		if len(clients) == 0 {
			d := &net.Dialer{Timeout: timeout}
			raw, err = d.DialContext(ctx, "tcp", hop.Addr)
		} else {
			raw, err = dialThrough(ctx, clients[len(clients)-1], hop.Addr, timeout)
		}
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("连接跳板 %s 失败: %w", hop.Addr, err)
		}

		addr := hop.Addr
		store := cfg.HostKeys
		clientCfg := &ssh.ClientConfig{
			User: hop.User,
			Auth: auths,
			HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
				return store.Verify(addr, key)
			},
		}

		// 握手同样要有界：中间跳板黑洞时 NewClientConn 会一直等下去。
		_ = raw.SetDeadline(time.Now().Add(timeout))
		sc, chans, reqs, err := ssh.NewClientConn(raw, hop.Addr, clientCfg)
		_ = raw.SetDeadline(time.Time{})
		if err != nil {
			_ = raw.Close()
			closeAll()
			return nil, fmt.Errorf("SSH 握手失败（%s）: %w", hop.Addr, err)
		}
		clients = append(clients, ssh.NewClient(sc, chans, reqs))
	}

	conn, err := dialThrough(ctx, clients[len(clients)-1], target, timeout)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("经隧道连接 %s 失败: %w", target, err)
	}

	return &Tunnel{conn: conn, clients: clients}, nil
}

// dialThrough 经上一跳拨号，并给拨号加上界。
//
// ssh.Client.Dial 不接受 context，只能用 goroutine + select 兜一层。
// 超时后底层拨号可能仍在后台直到 TCP 自己放弃，属于已知的少量泄漏；
// 换来的是「中间跳板黑洞时 CLI 不会永久卡死」。
func dialThrough(ctx context.Context, c *ssh.Client, addr string, timeout time.Duration) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := c.Dial("tcp", addr)
		ch <- result{conn: conn, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case r := <-ch:
		return r.conn, r.err
	case <-timer.C:
		return nil, fmt.Errorf("拨号超时（%s）", timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func authMethods(hop Hop) ([]ssh.AuthMethod, error) {
	switch hop.AuthKind {
	case AuthPassword, "":
		if len(hop.Password) == 0 {
			return nil, fmt.Errorf("跳板 %s 缺少密码", hop.Addr)
		}
		return []ssh.AuthMethod{ssh.Password(string(hop.Password))}, nil

	case AuthPrivateKey:
		if len(hop.PrivateKey) == 0 {
			return nil, fmt.Errorf("跳板 %s 缺少私钥", hop.Addr)
		}
		var (
			signer ssh.Signer
			err    error
		)
		if len(hop.Passphrase) > 0 {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(hop.PrivateKey, hop.Passphrase)
		} else {
			signer, err = ssh.ParsePrivateKey(hop.PrivateKey)
		}
		if err != nil {
			return nil, fmt.Errorf("解析私钥失败: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil

	case AuthAgent:
		if hop.AgentSocket == "" {
			return nil, fmt.Errorf("跳板 %s 未指定 agent socket", hop.Addr)
		}
		conn, err := net.Dial("unix", hop.AgentSocket)
		if err != nil {
			return nil, fmt.Errorf("连接 SSH agent 失败: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeysCallback(agent.NewClient(conn).Signers)}, nil
	}

	return nil, fmt.Errorf("不支持的认证方式: %s", hop.AuthKind)
}

// HostKeyMismatchError 表示主机密钥与已记录的不一致，可能是中间人攻击或服务器重建。
type HostKeyMismatchError struct {
	Addr string
	Want string
	Got  string
}

func (e *HostKeyMismatchError) Error() string {
	return fmt.Sprintf("主机密钥已变更（%s）：期望 %s，实际 %s", e.Addr, e.Want, e.Got)
}

// HostKeyUnknownError 表示首次见到该主机的密钥且未被授权接受。
//
// 默认拒绝是刻意的：静默信任首连等于给中间人开了门。
// 用户核对指纹后可通过 OnFirstUse 回调（或 CLI 的 -accept-host-key）显式接受。
type HostKeyUnknownError struct {
	Addr        string
	Fingerprint string
}

func (e *HostKeyUnknownError) Error() string {
	return fmt.Sprintf("主机 %s 的密钥尚未被信任（%s）。核对指纹后显式接受再重试", e.Addr, e.Fingerprint)
}

// OnFirstUse 在首次见到某主机密钥时被调用。返回 true 表示接受并记录。
type OnFirstUse func(addr, fingerprint string) bool

// MemoryHostKeys 是进程内的 TOFU 实现，用于测试与一次性命令。
type MemoryHostKeys struct {
	mu      sync.Mutex
	known   map[string]string
	onFirst OnFirstUse
	onNew   func(addr, fingerprint string) // 记录新指纹后回调（用于落盘）
}

// NewMemoryHostKeys 创建空的信任库。默认拒绝未知主机。
func NewMemoryHostKeys() *MemoryHostKeys {
	return &MemoryHostKeys{known: map[string]string{}}
}

// SetOnFirstUse 设置首连决策回调。不设置即「拒绝未知主机」。
func (m *MemoryHostKeys) SetOnFirstUse(fn OnFirstUse) {
	m.mu.Lock()
	m.onFirst = fn
	m.mu.Unlock()
}

// Verify 校验主机密钥：已记录必须匹配；未记录必须经 OnFirstUse 同意。
func (m *MemoryHostKeys) Verify(addr string, key ssh.PublicKey) error {
	fp := ssh.FingerprintSHA256(key)

	m.mu.Lock()
	prev, known := m.known[addr]
	onFirst := m.onFirst
	m.mu.Unlock()

	if known {
		if prev != fp {
			return &HostKeyMismatchError{Addr: addr, Want: prev, Got: fp}
		}
		return nil
	}

	// 首次见到：没有回调就拒绝。回调在无锁状态下调用，避免重入死锁。
	if onFirst == nil || !onFirst(addr, fp) {
		return &HostKeyUnknownError{Addr: addr, Fingerprint: fp}
	}

	m.mu.Lock()
	m.known[addr] = fp
	onNew := m.onNew
	m.mu.Unlock()

	if onNew != nil {
		onNew(addr, fp)
	}
	return nil
}

// Known 返回已记录的指纹，便于展示。
func (m *MemoryHostKeys) Known(addr string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fp, ok := m.known[addr]
	return fp, ok
}

// Entries 返回全部已知条目（副本）。
func (m *MemoryHostKeys) Entries() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.known))
	for k, v := range m.known {
		out[k] = v
	}
	return out
}
