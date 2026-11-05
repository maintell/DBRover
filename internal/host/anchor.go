package host

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/maintell/DBRover/internal/infra/secret"
)

// AnchorKey 取出（或首次生成）审计锚点用的密钥，存 OS 凭据库。
// 只在显式指定 -audit-anchor 时调用，不会在默认启动路径上偷偷写凭据库。
func AnchorKey() ([]byte, error) {
	const svc, acct = "dbrover/audit", "anchor-key"

	v, err := secret.Load(svc, acct)
	if err == nil && v != "" {
		return []byte(v), nil
	}
	if err != nil && !errors.Is(err, secret.ErrNotFound) {
		if errors.Is(err, secret.ErrUnsupported) {
			return nil, fmt.Errorf("审计锚点需要系统凭据库存放密钥，当前平台不支持")
		}
		return nil, err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("生成锚点密钥失败: %w", err)
	}
	hexKey := hex.EncodeToString(buf)
	if err := secret.Store(svc, acct, hexKey); err != nil {
		return nil, fmt.Errorf("锚点密钥存不进系统凭据库: %w", err)
	}
	fmt.Fprintf(os.Stderr, "已生成审计锚点密钥并存入系统凭据库：%s/%s\n", svc, acct)
	return []byte(hexKey), nil
}
