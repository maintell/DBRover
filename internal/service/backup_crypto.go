package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// 备份加密的参数（docs/08 §7、ADR-0010）。
//
// 选型理由：只用标准库（crypto/aes + crypto/pbkdf2），不引第三方；口令由用户
// 每次输入，不落盘、不进凭据库——备份文件常被拷到别的机器，绑本机凭据库反而
// 解不开。
const (
	backupKDF            = "pbkdf2-sha256"
	backupCipher         = "aes-256-gcm"
	backupKDFIterations  = 210000
	backupSaltLen        = 16
	backupNonceLen       = 12
	backupDerivedKeySize = 32
)

// ErrBackupNeedPassphrase 表示容器加密了但没给口令。
var ErrBackupNeedPassphrase = errors.New("这是加密容器，需要口令")

// ErrBackupBadPassphrase 表示口令不对，或密文被改过。
//
// AEAD 无法区分「口令错」与「密文被篡改」——两者都表现为认证失败。错误文案
// 必须把两种可能都说出来，否则用户会以为文件坏了。
var ErrBackupBadPassphrase = errors.New("口令不对，或文件已被改动")

// newBackupSalt 生成随机盐并写进头部。
func newBackupSalt() ([]byte, string, error) {
	salt := make([]byte, backupSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, "", err
	}
	return salt, base64.StdEncoding.EncodeToString(salt), nil
}

// deriveBackupKey 用口令 + 盐派生 32 字节密钥。
func deriveBackupKey(passphrase string, salt []byte, iterations int) ([]byte, error) {
	if iterations <= 0 {
		iterations = backupKDFIterations
	}
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, backupDerivedKeySize)
	if err != nil {
		return nil, fmt.Errorf("派生密钥: %w", err)
	}
	return key, nil
}

// encryptSection 返回 nonce || 密文。
func encryptSection(key []byte, plain []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, backupNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := gcm.Seal(nil, nonce, plain, nil)
	return append(nonce, out...), nil
}

// decryptSection 拆出 nonce 并解密。
func decryptSection(key []byte, data []byte) ([]byte, error) {
	if len(data) < backupNonceLen {
		return nil, fmt.Errorf("%w: 密文长度不足", ErrBackupFormat)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, data[:backupNonceLen], data[backupNonceLen:], nil)
	if err != nil {
		return nil, ErrBackupBadPassphrase
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// backupKey 是「写入端」的密钥材料：加密开启时非空。
type backupKey struct {
	key []byte
}

// writeKeyForHeader 是写入端的密钥：头部已经决定是否加密。
//
// 续跑时必须用文件里的盐重新派生，不能沿用本次请求——否则同一个文件里会出现
// 两把钥匙加密的段，读端只能解一半。
func writeKeyForHeader(h *BackupHeader, passphrase string) ([]byte, error) {
	if h.Flags&backupFlagEncrypted == 0 && h.KDF == "" {
		return nil, nil
	}
	if passphrase == "" {
		return nil, ErrBackupNeedPassphrase
	}
	salt, err := base64.StdEncoding.DecodeString(h.Salt)
	if err != nil {
		return nil, fmt.Errorf("%w: 盐不是合法 base64", ErrBackupFormat)
	}
	return deriveBackupKey(passphrase, salt, h.Iterations)
}

// restoreKey 是「读取端」的密钥材料，按头部参数派生。
func restoreKeyFromHeader(h *BackupHeader, passphrase string) ([]byte, error) {
	if h.Flags&backupFlagEncrypted == 0 {
		return nil, nil
	}
	if passphrase == "" {
		return nil, ErrBackupNeedPassphrase
	}
	if h.KDF != backupKDF || h.Cipher != backupCipher {
		return nil, fmt.Errorf("%w: 不认识的加密参数（kdf=%q cipher=%q）", ErrBackupFormat, h.KDF, h.Cipher)
	}
	salt, err := base64.StdEncoding.DecodeString(h.Salt)
	if err != nil {
		return nil, fmt.Errorf("%w: 盐不是合法 base64", ErrBackupFormat)
	}
	return deriveBackupKey(passphrase, salt, h.Iterations)
}
