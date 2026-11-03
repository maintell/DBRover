//go:build !windows

package secret

// 非 Windows 平台先明确「不支持」，而不是悄悄退化成明文文件。
//
// 退化成文件是最糟的选择：调用方以为凭据进了系统库，实际落在磁盘上，
// 而且没人会再去 review 它。这里宁可让功能不可用，也不制造一个假的安心。
// Linux（libsecret）与 macOS（Keychain）的实现各自要引 CGO 或命令行工具，
// 放到有对应验证环境的机器上再做。

// Store 在非 Windows 平台上不可用。
func Store(service, account, value string) error {
	if err := validate(service, account); err != nil {
		return err
	}
	return ErrUnsupported
}

// Load 在非 Windows 平台上不可用。
func Load(service, account string) (string, error) {
	if err := validate(service, account); err != nil {
		return "", err
	}
	return "", ErrUnsupported
}

// Delete 在非 Windows 平台上不可用。
func Delete(service, account string) error {
	if err := validate(service, account); err != nil {
		return err
	}
	return ErrUnsupported
}
