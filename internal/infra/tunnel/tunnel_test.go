package tunnel_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/maintell/DBRover/internal/infra/tunnel"
)

// 服务端回调的具名类型（ssh 包里这些是函数字段而不是类型）。
type passwordCB func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error)
type publicKeyCB func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error)

// startBastion 起一个真实的 SSH 服务端，支持 direct-tcpip 转发。
// 用库自带的服务端实现，链路是真实的 SSH 协议，不是 mock。
func startBastion(t *testing.T, user string, passwordCallback passwordCB, keyCallback publicKeyCB) *bastion {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成主机密钥: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("构造 signer: %v", err)
	}

	sc := &ssh.ServerConfig{
		PasswordCallback:  passwordCallback,
		PublicKeyCallback: keyCallback,
	}
	sc.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSSH(conn, sc)
		}
	}()

	return &bastion{addr: ln.Addr().String(), hostKey: signer.PublicKey()}
}

type bastion struct {
	addr    string
	hostKey ssh.PublicKey
}

// trustingStore 返回一个「首连即接受」的信任库，供不需要验证 TOFU 流程的用例使用。
func trustingStore() *tunnel.MemoryHostKeys {
	s := tunnel.NewMemoryHostKeys()
	s.SetOnFirstUse(func(addr, fingerprint string) bool { return true })
	return s
}

func passwordAuth(user, pass string) passwordCB {
	return func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if c.User() == user && string(p) == pass {
			return nil, nil
		}
		return nil, errors.New("认证失败")
	}
}

func serveSSH(conn net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)

	for nc := range chans {
		if nc.ChannelType() != "direct-tcpip" {
			_ = nc.Reject(ssh.UnknownChannelType, "仅支持 direct-tcpip")
			continue
		}
		go forwardChannel(nc)
	}
}

type directTCPIP struct {
	Host       string
	Port       uint32
	OriginHost string
	OriginPort uint32
}

func forwardChannel(nc ssh.NewChannel) {
	var p directTCPIP
	if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, "无法解析转发参数")
		return
	}
	target := net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port)))
	dconn, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		_ = dconn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() {
		_, _ = io.Copy(ch, dconn)
		_ = ch.Close()
	}()
	_, _ = io.Copy(dconn, ch)
	_ = dconn.Close()
}

func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听 echo: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func assertEcho(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatalf("写入: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("读取: %v", err)
	}
	if string(buf) != msg {
		t.Errorf("回显 = %q, 期望 %q", buf, msg)
	}
}

func TestDialSingleHop(t *testing.T) {
	b := startBastion(t, "ops", passwordAuth("ops", "secret"), nil)
	echo := startEcho(t)

	tr, err := tunnel.Dial(context.Background(), tunnel.Config{
		Hops:     []tunnel.Hop{{Addr: b.addr, User: "ops", AuthKind: tunnel.AuthPassword, Password: []byte("secret")}},
		HostKeys: trustingStore(),
	}, echo)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = tr.Close() }()

	if tr.Hops() != 1 {
		t.Errorf("Hops = %d, 期望 1", tr.Hops())
	}
	assertEcho(t, tr.Conn(), "ping")
}

func TestDialTwoHops(t *testing.T) {
	b1 := startBastion(t, "ops", passwordAuth("ops", "s1"), nil)
	b2 := startBastion(t, "ops", passwordAuth("ops", "s2"), nil)
	echo := startEcho(t)

	tr, err := tunnel.Dial(context.Background(), tunnel.Config{
		Hops: []tunnel.Hop{
			{Addr: b1.addr, User: "ops", AuthKind: tunnel.AuthPassword, Password: []byte("s1")},
			{Addr: b2.addr, User: "ops", AuthKind: tunnel.AuthPassword, Password: []byte("s2")},
		},
		HostKeys: trustingStore(),
	}, echo)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = tr.Close() }()

	if tr.Hops() != 2 {
		t.Errorf("Hops = %d, 期望 2", tr.Hops())
	}
	assertEcho(t, tr.Conn(), "two-hop-payload")
}

func TestDialWithPrivateKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("公钥: %v", err)
	}
	keyCallback := func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(k.Marshal(), sshPub.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("公钥不匹配")
	}

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("编码私钥: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	b := startBastion(t, "deploy", nil, keyCallback)
	echo := startEcho(t)

	tr, err := tunnel.Dial(context.Background(), tunnel.Config{
		Hops: []tunnel.Hop{{
			Addr: b.addr, User: "deploy",
			AuthKind: tunnel.AuthPrivateKey, PrivateKey: pemKey,
		}},
		HostKeys: trustingStore(),
	}, echo)
	if err != nil {
		t.Fatalf("Dial(私钥): %v", err)
	}
	defer func() { _ = tr.Close() }()
	assertEcho(t, tr.Conn(), "key-auth")
}

func TestDialWrongPassword(t *testing.T) {
	b := startBastion(t, "ops", passwordAuth("ops", "secret"), nil)
	echo := startEcho(t)

	_, err := tunnel.Dial(context.Background(), tunnel.Config{
		Hops:     []tunnel.Hop{{Addr: b.addr, User: "ops", AuthKind: tunnel.AuthPassword, Password: []byte("wrong")}},
		HostKeys: trustingStore(),
	}, echo)
	if err == nil {
		t.Fatal("错误密码应导致握手失败")
	}
}

// TestHostKeyMismatchRejected 验证 TOFU：已记录指纹后换主机密钥必须被拒。
func TestHostKeyMismatchRejected(t *testing.T) {
	real := startBastion(t, "ops", passwordAuth("ops", "secret"), nil)
	other := startBastion(t, "ops", passwordAuth("ops", "secret"), nil)
	echo := startEcho(t)

	store := trustingStore()
	// 先把「错误」的指纹记到真实跳板的地址上，模拟中间人或服务器重建。
	if err := store.Verify(real.addr, other.hostKey); err != nil {
		t.Fatalf("首次记录应成功: %v", err)
	}

	_, err := tunnel.Dial(context.Background(), tunnel.Config{
		Hops:     []tunnel.Hop{{Addr: real.addr, User: "ops", AuthKind: tunnel.AuthPassword, Password: []byte("secret")}},
		HostKeys: store,
	}, echo)
	if err == nil {
		t.Fatal("主机密钥不匹配时必须拒绝连接")
	}
	var mismatch *tunnel.HostKeyMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("错误类型 = %v, 期望 HostKeyMismatchError", err)
	}
}

func TestDialRequiresHostKeyStore(t *testing.T) {
	_, err := tunnel.Dial(context.Background(), tunnel.Config{
		Hops: []tunnel.Hop{{Addr: "127.0.0.1:1", User: "x", AuthKind: tunnel.AuthPassword, Password: []byte("y")}},
	}, "127.0.0.1:2")
	if err == nil {
		t.Fatal("缺少主机密钥校验实现时应拒绝")
	}
}

func TestDialRejectsTooManyHops(t *testing.T) {
	hops := make([]tunnel.Hop, tunnel.MaxHops+1)
	for i := range hops {
		hops[i] = tunnel.Hop{Addr: "127.0.0.1:1", User: "x", AuthKind: tunnel.AuthPassword, Password: []byte("y")}
	}
	_, err := tunnel.Dial(context.Background(), tunnel.Config{
		Hops: hops, HostKeys: trustingStore(),
	}, "127.0.0.1:2")
	if err == nil {
		t.Fatal("超过跳数上限应报错")
	}
}

func TestMemoryHostKeysPins(t *testing.T) {
	b := startBastion(t, "ops", passwordAuth("ops", "secret"), nil)
	store := trustingStore()

	if err := store.Verify(b.addr, b.hostKey); err != nil {
		t.Fatalf("确认后应接受: %v", err)
	}
	if err := store.Verify(b.addr, b.hostKey); err != nil {
		t.Fatalf("相同指纹应接受: %v", err)
	}
	fp, ok := store.Known(b.addr)
	if !ok || fp == "" {
		t.Fatal("应记录指纹")
	}
}

// TestHostKeyRejectsUnknownByDefault 是安全回归：
// 首次见到的密钥在没有显式同意时必须拒绝，否则首连遇中间人会被静默信任。
func TestHostKeyRejectsUnknownByDefault(t *testing.T) {
	b := startBastion(t, "ops", passwordAuth("ops", "secret"), nil)
	echo := startEcho(t)

	_, err := tunnel.Dial(context.Background(), tunnel.Config{
		Hops: []tunnel.Hop{{
			Addr: b.addr, User: "ops",
			AuthKind: tunnel.AuthPassword, Password: []byte("secret"),
		}},
		HostKeys: tunnel.NewMemoryHostKeys(), // 没有 OnFirstUse
	}, echo)
	if err == nil {
		t.Fatal("未信任的主机必须拒绝")
	}
	var unknown *tunnel.HostKeyUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("错误类型 = %v, 期望 HostKeyUnknownError", err)
	}
	if unknown.Fingerprint == "" {
		t.Error("错误里应带上指纹，便于用户核对")
	}
}

// TestHostKeyRejectCallbackBlocks 验证回调返回 false 时同样拒绝。
func TestHostKeyRejectCallbackBlocks(t *testing.T) {
	store := tunnel.NewMemoryHostKeys()
	store.SetOnFirstUse(func(addr, fp string) bool { return false })

	b := startBastion(t, "ops", passwordAuth("ops", "secret"), nil)
	if err := store.Verify(b.addr, b.hostKey); err == nil {
		t.Fatal("回调拒绝时 Verify 必须报错")
	}
	if _, ok := store.Known(b.addr); ok {
		t.Error("被拒绝的指纹不应被记录")
	}
}

// TestFileHostKeysPersists 验证信任落盘后重启不失忆。
func TestFileHostKeysPersists(t *testing.T) {
	b := startBastion(t, "ops", passwordAuth("ops", "secret"), nil)
	path := filepath.Join(t.TempDir(), "known_hosts.json")

	first, err := tunnel.NewFileHostKeys(path)
	if err != nil {
		t.Fatalf("NewFileHostKeys: %v", err)
	}
	first.SetOnFirstUse(func(addr, fp string) bool { return true })
	if err := first.Verify(b.addr, b.hostKey); err != nil {
		t.Fatalf("首次应接受: %v", err)
	}

	// 重新打开：不需要回调也应通过，因为已落盘。
	second, err := tunnel.NewFileHostKeys(path)
	if err != nil {
		t.Fatalf("重开信任库: %v", err)
	}
	if err := second.Verify(b.addr, b.hostKey); err != nil {
		t.Fatalf("重启后应仍然信任: %v", err)
	}

	// 换密钥必须被拒。
	other := startBastion(t, "ops", passwordAuth("ops", "secret"), nil)
	var mismatch *tunnel.HostKeyMismatchError
	if err := second.Verify(b.addr, other.hostKey); !errors.As(err, &mismatch) {
		t.Fatalf("换密钥应报 HostKeyMismatchError, 实际 %v", err)
	}
}

// TestHandshakeTimeout 覆盖「中间跳板黑洞时卡死」：
// 用只 accept、不回任何字节的假跳板，验证握手会被 deadline 打断。
func TestHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听: %v", err)
	}
	defer func() { _ = ln.Close() }()

	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// 故意不读不写也不关闭，模拟黑洞跳板。
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	}()

	started := time.Now()
	_, err = tunnel.Dial(context.Background(), tunnel.Config{
		Hops: []tunnel.Hop{{
			Addr: ln.Addr().String(), User: "ops",
			AuthKind: tunnel.AuthPassword, Password: []byte("x"),
		}},
		HostKeys: trustingStore(),
		Timeout:  300 * time.Millisecond,
	}, "127.0.0.1:1")

	if err == nil {
		t.Fatal("对黑洞跳板应握手失败")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("握手未被超时打断，耗时 %s", elapsed)
	}
}
