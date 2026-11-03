//go:build windows

package secret

import (
	"syscall"
	"unsafe"
)

// Windows 凭据管理器（advapi32）的最小封装。
//
// 为什么不用第三方库：x/sys/windows 没有封装 Cred*，而引一个纯为这件事存在的
// 依赖要过 ADR；这里的结构体是文档里写死的布局，几十行就够，且能自己写测试。
var (
	advapi32   = syscall.NewLazyDLL("advapi32.dll")
	credRead   = advapi32.NewProc("CredReadW")
	credWrite  = advapi32.NewProc("CredWriteW")
	credDelete = advapi32.NewProc("CredDeleteW")
	credFree   = advapi32.NewProc("CredFree")
)

const (
	credTypeGeneric             = 1 // CRED_TYPE_GENERIC
	credPersistLocalMach        = 2 // CRED_PERSIST_LOCAL_MACHINE
	errNotFound          uint32 = 1168
)

// credentialW 对应 CREDENTIALW。字段顺序与对齐必须与 Windows SDK 一致——
// Go 在 amd64 上的对齐规则与 MSVC 相同，所以直接照抄结构体即可。
type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// Store 写入（或覆盖）一条凭据。
func Store(service, account, value string) error {
	if err := validate(service, account); err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(Target(service, account))
	if err != nil {
		return redactErr("编码目标名", err)
	}
	user, err := syscall.UTF16PtrFromString(account)
	if err != nil {
		return redactErr("编码账号", err)
	}
	blob := []byte(value)
	if len(blob) == 0 {
		// 空口令也要能存：CredWrite 允许 size=0，但要让指针非空，
		// 否则某些版本会当成「没给 blob」。
		blob = []byte{0}
		blob = blob[:0]
	}

	cred := credentialW{
		Type:               credTypeGeneric,
		TargetName:         target,
		CredentialBlobSize: uint32(len(blob)),
		Persist:            credPersistLocalMach,
		UserName:           user,
	}
	if len(blob) > 0 {
		cred.CredentialBlob = &blob[0]
	}

	r, _, errno := credWrite.Call(uintptr(unsafe.Pointer(&cred)), 0)
	if r == 0 {
		return redactErr("写入凭据库", errno)
	}
	return nil
}

// Load 读取一条凭据。
func Load(service, account string) (string, error) {
	if err := validate(service, account); err != nil {
		return "", err
	}
	target, err := syscall.UTF16PtrFromString(Target(service, account))
	if err != nil {
		return "", redactErr("编码目标名", err)
	}

	var pcred *credentialW
	r, _, errno := credRead.Call(
		uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&pcred)))
	if r == 0 {
		if errno == syscall.Errno(errNotFound) {
			return "", ErrNotFound
		}
		return "", redactErr("读取凭据库", errno)
	}
	defer credFree.Call(uintptr(unsafe.Pointer(pcred)))

	if pcred == nil || pcred.CredentialBlobSize == 0 {
		return "", nil
	}
	blob := unsafe.Slice(pcred.CredentialBlob, pcred.CredentialBlobSize)
	return string(blob), nil
}

// Delete 删除一条凭据。不存在时返回 ErrNotFound，调用方自行决定是否忽略。
func Delete(service, account string) error {
	if err := validate(service, account); err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(Target(service, account))
	if err != nil {
		return redactErr("编码目标名", err)
	}
	r, _, errno := credDelete.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
	if r == 0 {
		if errno == syscall.Errno(errNotFound) {
			return ErrNotFound
		}
		return redactErr("删除凭据", errno)
	}
	return nil
}
