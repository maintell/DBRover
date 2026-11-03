// Package secret 把凭据存进操作系统的凭据库，而不是明文文件或环境变量。
//
// 为什么不让调用方自己决定存哪：docs/10 §2 要求「密码/私钥/口令只存 OS 凭据库」，
// 这条规则如果交给每个调用点自己实现，一定会有一处图省事写成明文。
package secret

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrNotFound 表示这条凭据不存在（不是错误，是需要区分「没配」和「读失败」）。
	ErrNotFound = errors.New("secret: 凭据不存在")
	// ErrUnsupported 表示当前平台没有可用的凭据库实现。
	ErrUnsupported = errors.New("secret: 当前平台不支持系统凭据库")
	// ErrEmpty 表示参数不合法。
	ErrEmpty = errors.New("secret: service 与 account 不能为空")
)

// Service 是凭据的命名空间，例如 "dbrover/agent"。
//
// 用斜杠而不是点：Windows 凭据管理器里显示成一条普通的目标名，
// 点号容易被误认成域名。
const (
	// AgentAPIKeyService / Account 是 Agent 模型服务 API Key 的存法。
	AgentAPIKeyService = "dbrover/agent"
	AgentAPIKeyAccount = "api-key"
)

// Target 把 service 与 account 拼成凭据库里的目标名。
//
// Windows 的 Credential Manager 用「目标名 + 用户名」两级定位；这里约定
// 目标名 = service，用户名 = account，两侧语义与其它平台一致。
func Target(service, account string) string {
	return strings.TrimRight(service, "/") + "/" + account
}

// validate 统一挡掉空参数。
func validate(service, account string) error {
	if strings.TrimSpace(service) == "" || strings.TrimSpace(account) == "" {
		return ErrEmpty
	}
	return nil
}

// redactErr 把错误里可能带出的凭据内容去掉，只留错误类型。
//
// 凭据库的错误信息通常不含口令，但这里宁可少说：这条路径上的错误会进日志。
func redactErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnsupported) {
		return err
	}
	return fmt.Errorf("secret: %s 失败: %w", op, err)
}
