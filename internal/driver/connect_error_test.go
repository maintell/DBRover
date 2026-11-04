package driver_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
)

// timeoutErr 是一个只实现 net.Error 的超时错误。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// TestClassifyNetError 守住网络层失败归类（FR-CON-03 的第一层）。
//
// 这一层与引擎无关，三个驱动共用。分错类的后果是建议指向完全错误的方向：
// 把「口令错」说成「端口不通」，人会去查防火墙，永远不会去核口令。
func TestClassifyNetError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want driver.ConnectErrorKind
	}{
		{"域名解析", &net.DNSError{Err: "no such host", Name: "nope.invalid"}, driver.ConnectDNS},
		{"连接被拒", &net.OpError{Op: "dial", Net: "tcp",
			Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, driver.ConnectRefused},
		{"超时", &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}, driver.ConnectTimeout},
		{"其他", errors.New("something else"), driver.ConnectOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := driver.ClassifyNetError(c.err); got != c.want {
				t.Errorf("ClassifyNetError = %q, 期望 %q", got, c.want)
			}
		})
	}

	// 握手被切断（真机上很常见：中间设备不拒绝也不超时，只给一个 EOF）。
	if got := driver.ClassifyNetError(fmt.Errorf("startup: %w", io.ErrUnexpectedEOF)); got != driver.ConnectHandshake {
		t.Errorf("unexpected EOF = %q, 期望 handshake", got)
	}

	// context 超时也要归到超时：驱动用的是带 10 秒 deadline 的 ping。
	if got := driver.ClassifyNetError(fmt.Errorf("ping: %w", context.DeadlineExceeded)); got != driver.ConnectTimeout {
		t.Errorf("deadline = %q, 期望 timeout", got)
	}
}

// TestClassifyDialFailure 覆盖 Windows 上的形态：内核给的是 WSAECONNREFUSED，
// 与 syscall.ECONNREFUSED 不是同一个值，errors.Is 匹配不到（真机踩到过，
// 表现是端口关了却报「连接失败」而不是「端口不通」）。
// 这时靠 *net.OpError 的 Op 判定——拨号阶段失败已经足够定位问题。
func TestClassifyDialFailure(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connectex: refused")}
	if got := driver.ClassifyNetError(dialErr); got != driver.ConnectRefused {
		t.Errorf("拨号失败 = %q, 期望 refused", got)
	}
	// 读失败不该被误判成拨号失败。
	rw := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("eof")}
	if got := driver.ClassifyNetError(rw); got != driver.ConnectOther {
		t.Errorf("读失败 = %q, 期望 other", got)
	}
}

// TestConnectErrorUnwrap 保证分类包装之后，调用方仍能拿到驱动原始错误。
//
// 这条不能省：上层有时要按原始错误类型再判一次（比如引擎特有的错误号），
// 包装如果吃掉了 Unwrap，那种判断就会静默失效。
func TestConnectErrorUnwrap(t *testing.T) {
	orig := errors.New("boom")
	err := &driver.ConnectError{Kind: driver.ConnectAuth, Err: orig}
	if !errors.Is(err, orig) {
		t.Fatal("ConnectError 丢了原始错误，errors.Is 找不到它")
	}
	var ce *driver.ConnectError
	if !errors.As(err, &ce) || ce.Kind != driver.ConnectAuth {
		t.Fatalf("errors.As 没认出 ConnectError：%+v", ce)
	}
}
