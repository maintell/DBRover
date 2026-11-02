package tunnel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// FileHostKeys 是落盘的信任库（JSON）。
//
// 落盘的意义：进程重启后信任不丢。只存内存时，重启即忘，
// 被替换的主机永远不会触发「密钥已变更」告警。
type FileHostKeys struct {
	*MemoryHostKeys
	path string
}

// NewFileHostKeys 打开（或创建）信任库文件。文件不存在视为空库。
func NewFileHostKeys(path string) (*FileHostKeys, error) {
	if path == "" {
		return nil, fmt.Errorf("信任库路径不能为空")
	}
	f := &FileHostKeys{MemoryHostKeys: NewMemoryHostKeys(), path: path}

	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		var entries map[string]string
		if err := json.Unmarshal(b, &entries); err != nil {
			return nil, fmt.Errorf("解析信任库 %s: %w", path, err)
		}
		f.MemoryHostKeys.mu.Lock()
		for k, v := range entries {
			f.MemoryHostKeys.known[k] = v
		}
		f.MemoryHostKeys.mu.Unlock()
	case os.IsNotExist(err):
		// 空库
	default:
		return nil, fmt.Errorf("读取信任库 %s: %w", path, err)
	}

	// 记录新指纹后立即落盘。
	f.MemoryHostKeys.onNew = func(_, _ string) {
		if err := f.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "写入信任库失败: %v\n", err)
		}
	}
	return f, nil
}

// Path 返回信任库文件路径。
func (f *FileHostKeys) Path() string { return f.path }

// Save 把当前信任库写回文件（权限 0600；Windows 依赖目录 ACL）。
func (f *FileHostKeys) Save() error {
	entries := f.Entries()
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(f.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建信任库目录: %w", err)
		}
	}
	if err := os.WriteFile(f.path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入信任库: %w", err)
	}
	return nil
}
