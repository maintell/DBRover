package settings

import (
	"errors"
	"fmt"
	"sync"

	"github.com/maintell/DBRover/internal/infra/secret"
)

// MemStore 是纯内存实现：设置文件不可用时 host 用它顶上，本次进程内可改但不落盘。
// 与 FileStore 共用 applyPatch，校验规则只有一份。
type MemStore struct {
	mu  sync.Mutex
	cur Settings
	key string
}

func NewMemStore(d Settings) *MemStore {
	d.APIKeyConfigured = false
	return &MemStore{cur: d}
}

func (m *MemStore) Load() (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cur.APIKeyConfigured = m.key != ""
	return m.cur, nil
}

func (m *MemStore) Update(p Patch) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next, err := applyPatch(m.cur, p)
	if err != nil {
		return m.cur, err
	}
	if p.APIKey != nil {
		if *p.APIKey == "" {
			m.key = ""
		} else {
			m.key = *p.APIKey
		}
	}
	m.cur = next
	m.cur.APIKeyConfigured = m.key != ""
	return m.cur, nil
}

func (m *MemStore) APIKey() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.key, nil
}

// 让编译期确认 MemStore 没有悄悄丢掉接口方法（含未使用的错误变量占位）。
var _ = errors.Is
var _ = fmt.Errorf
var _ Store = (*MemStore)(nil)
var _ = secret.ErrNotFound
