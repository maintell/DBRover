package driver

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"syscall"
)

// ConnectErrorKind 是连接失败的**可诊断类别**（FR-CON-03）。
//
// 为什么要分类，而不是把驱动原始报错直接甩给用户：网络层的原始错误（连接被拒、
// 解析失败）对懂网络的人是一句话，对大多数人等于没说——他需要知道的是「服务没
// 起来」「端口不对」还是「口令错了」。分类放在**驱动**里做，因为引擎的错误码只有
// 驱动认得；上层只按类别给文案，不需要知道是哪个引擎（ADR-0005）。
type ConnectErrorKind string

const (
	// ConnectDNS 是域名解析失败。
	ConnectDNS ConnectErrorKind = "dns"
	// ConnectRefused 是端口拒绝连接（服务没起来、端口不对、被防火墙拒）。
	ConnectRefused ConnectErrorKind = "refused"
	// ConnectTimeout 是连接超时（网络不通或被中间设备丢弃）。
	ConnectTimeout ConnectErrorKind = "timeout"
	// ConnectAuth 是认证失败（用户名/口令错，或该用户不允许从这个来源登录）。
	ConnectAuth ConnectErrorKind = "auth"
	// ConnectNoDatabase 是库不存在（PG 的 database / MySQL 的 database）。
	ConnectNoDatabase ConnectErrorKind = "database_missing"
	// ConnectTLS 是 TLS 握手失败（证书、CA、加密模式与实例要求不一致）。
	ConnectTLS ConnectErrorKind = "tls"
	// ConnectHandshake 是 TCP 通了但握手被切断（对端不是这个引擎、代理/负载均衡
	// 把连接掐了、或者服务在处理启动包时就出错）。真机上很常见：中间设备不返回
	// 拒绝也不返回超时，只给一个 EOF，用户完全看不出发生了什么。
	ConnectHandshake ConnectErrorKind = "handshake"
	// ConnectFile 是本地文件打不开（SQLite 的路径、权限、只读）。
	ConnectFile ConnectErrorKind = "file"
	// ConnectOther 是没归类到上面的失败。上层会回落到通用错误码。
	ConnectOther ConnectErrorKind = "other"
)

// ConnectError 给连接失败带上类别，同时保留原始错误（Unwrap 之后仍然可以
// errors.Is / errors.As 到驱动自己的类型）。
type ConnectError struct {
	Kind ConnectErrorKind
	Err  error
}

func (e *ConnectError) Error() string { return e.Err.Error() }

// Unwrap 保证包装之后，调用方依然能拿到驱动原始错误（例如 *pgconn.PgError）。
func (e *ConnectError) Unwrap() error { return e.Err }

// ClassifyNetError 把网络层失败归类。三个驱动共用：这一层与引擎无关。
//
// 先看类型再看文本：Go 的 net 包给出的错误类型足够精确，只有最后兜底才碰字符串。
func ClassifyNetError(err error) ConnectErrorKind {
	if err == nil {
		return ConnectOther
	}
	// 超时（含 context deadline）。放最前面：超时错误里常带别的包装。
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return ConnectTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ConnectTimeout
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ConnectDNS
	}
	// 连接被拒：unix 上 errors.Is 就能命中。
	if errors.Is(err, syscall.ECONNREFUSED) {
		return ConnectRefused
	}
	// Windows 上内核给的是 WSAECONNREFUSED，与 syscall.ECONNREFUSED 不是同一个值，
	// errors.Is 匹配不到（实测 pgx 打出去的错误链里只剩 *net.OpError）。
	// 退一步看「是不是拨号阶段失败的」：那已经足以定位——服务没起来、端口不对，
	// 或这台机器到那个端口不通，三者给用户的动作是同一套。
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return ConnectRefused
	}
	// 握手被切断：TCP 已经通了，但对端在读/写启动包时直接关掉。
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ConnectHandshake
	}
	// 证书/握手失败。
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostnameErr) || errors.As(err, &certInvalid) {
		return ConnectTLS
	}
	return ConnectOther
}
