// Package settings 持久化非敏感的界面/模型设置，API Key 交给 OS 凭据库。
//
// 分两处的原因：docs/10 `3 要求「密码/私钥/口令只存 OS 凭据库」。设置文件是普通
// JSON，会被备份、被同步、被人打开看——把 API Key 放进去等于把凭据写进明文。
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/maintell/DBRover/internal/infra/secret"
)

const (
	// APIKeyService / APIKeyAccount 沿用 dbrover secret 的既有目标，
	// 设置面板与 dbrover secret set 写的是同一把 key，不会出现两份。
	APIKeyService = secret.AgentAPIKeyService
	APIKeyAccount = secret.AgentAPIKeyAccount
)

// Settings 是对外（含 UI）的设置视图。
// APIKeyConfigured 是派生字段，不写进文件：每次 Load 现查凭据库。
type Settings struct {
	Theme              string `json:"theme"`
	FontSize           int    `json:"font_size"`
	EditorTabSize      int    `json:"editor_tab_size"`
	AgentBaseURL       string `json:"agent_base_url"`
	AgentModel         string `json:"agent_model"`
	AgentContextTokens int    `json:"agent_context_tokens"`
	APIKeyConfigured   bool   `json:"api_key_configured"`
}

func Defaults() Settings {
	return Settings{Theme: "dark", FontSize: 13, EditorTabSize: 2}
}

// Patch 是 PATCH /settings 的请求体。指针字段区分「没给」与「给成零值」。
type Patch struct {
	Theme              *string `json:"theme"`
	FontSize           *int    `json:"font_size"`
	EditorTabSize      *int    `json:"editor_tab_size"`
	AgentBaseURL       *string `json:"agent_base_url"`
	AgentModel         *string `json:"agent_model"`
	AgentContextTokens *int    `json:"agent_context_tokens"`
	// APIKey 写进 OS 凭据库，绝不进设置文件；空串表示清除。
	APIKey *string `json:"api_key"`
}

// fileData 是落盘形状：刻意不含任何凭据字段。
type fileData struct {
	Theme              string `json:"theme"`
	FontSize           int    `json:"font_size"`
	EditorTabSize      int    `json:"editor_tab_size"`
	AgentBaseURL       string `json:"agent_base_url"`
	AgentModel         string `json:"agent_model"`
	AgentContextTokens int    `json:"agent_context_tokens"`
}

// KeyStore 把凭据库抽象成接口，单测换成内存实现，不碰系统凭据管理器。
type KeyStore interface {
	Store(service, account, value string) error
	Load(service, account string) (string, error)
	Delete(service, account string) error
}

type osKeys struct{}

func (osKeys) Store(s, a, v string) error       { return secret.Store(s, a, v) }
func (osKeys) Load(s, a string) (string, error) { return secret.Load(s, a) }
func (osKeys) Delete(s, a string) error         { return secret.Delete(s, a) }

// Store 是 api 层依赖的设置读写面（host 注入具体实现）。
type Store interface {
	Load() (Settings, error)
	Update(Patch) (Settings, error)
	APIKey() (string, error)
}

// DefaultPath 返回跨桌面/团队模式共用的默认配置位置。
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "dbrover", "settings.json")
}

// FileStore 用单文件 JSON 存非敏感项，用凭据库存 API Key。
type FileStore struct {
	mu   sync.Mutex
	path string
	cur  Settings
	keys KeyStore
}

func NewFileStore(path string) (*FileStore, error) {
	return newFileStore(path, osKeys{})
}

// newFileStore 读不到或读不懂文件时**不报错**：明确告警后用默认设置启动，
// 下一次 Update 会把损坏的文件覆盖修复。应用不该因为一份可有可无的配置打不开。
func newFileStore(path string, keys KeyStore) (*FileStore, error) {
	if path == "" {
		path = DefaultPath()
	}
	s := &FileStore{path: path, cur: Defaults(), keys: keys}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		var fd fileData
		if jerr := json.Unmarshal(b, &fd); jerr != nil {
			fmt.Fprintf(os.Stderr, "设置文件损坏，已用默认设置启动（下次保存会覆盖修复）：%v\n", jerr)
		} else {
			s.cur = Settings{
				Theme: fd.Theme, FontSize: fd.FontSize, EditorTabSize: fd.EditorTabSize,
				AgentBaseURL: fd.AgentBaseURL, AgentModel: fd.AgentModel,
				AgentContextTokens: fd.AgentContextTokens,
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// 首次启动：用默认值，不落盘，等真的改了再写。
	default:
		fmt.Fprintf(os.Stderr, "设置文件读不到，已用默认设置启动：%v\n", err)
	}
	if s.cur.Theme == "" {
		s.cur.Theme = Defaults().Theme
	}
	if s.cur.FontSize == 0 {
		s.cur.FontSize = Defaults().FontSize
	}
	if s.cur.EditorTabSize == 0 {
		s.cur.EditorTabSize = Defaults().EditorTabSize
	}
	s.cur.APIKeyConfigured = s.keyConfigured()
	return s, nil
}

func (s *FileStore) keyConfigured() bool {
	v, err := s.keys.Load(APIKeyService, APIKeyAccount)
	return err == nil && v != ""
}

func (s *FileStore) Load() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur.APIKeyConfigured = s.keyConfigured()
	return s.cur, nil
}

// applyPatch 是与存储后端无关的校验与合并，FileStore 与 MemStore 共用，
// 避免两处校验逻辑各自演化。
func applyPatch(cur Settings, p Patch) (Settings, error) {
	next := cur
	if p.Theme != nil {
		v := strings.ToLower(strings.TrimSpace(*p.Theme))
		if v != "dark" && v != "light" {
			return cur, fmt.Errorf("theme 只能是 dark 或 light")
		}
		next.Theme = v
	}
	if p.FontSize != nil {
		if *p.FontSize < 12 || *p.FontSize > 20 {
			return cur, fmt.Errorf("font_size 必须在 12..20")
		}
		next.FontSize = *p.FontSize
	}
	if p.EditorTabSize != nil {
		if *p.EditorTabSize < 2 || *p.EditorTabSize > 8 {
			return cur, fmt.Errorf("editor_tab_size 必须在 2..8")
		}
		next.EditorTabSize = *p.EditorTabSize
	}
	if p.AgentBaseURL != nil {
		next.AgentBaseURL = strings.TrimSpace(*p.AgentBaseURL)
	}
	if p.AgentModel != nil {
		next.AgentModel = strings.TrimSpace(*p.AgentModel)
	}
	if p.AgentContextTokens != nil {
		if *p.AgentContextTokens < 0 {
			return cur, fmt.Errorf("agent_context_tokens 不能为负")
		}
		next.AgentContextTokens = *p.AgentContextTokens
	}
	return next, nil
}

// Update 应用一次局部更新；校验放在这里，api 层只把错误映射成 400。
func (s *FileStore) Update(p Patch) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next, err := applyPatch(s.cur, p)
	if err != nil {
		return s.cur, err
	}
	// API Key 单独处理：空串清除、非空写凭据库，两者都不进设置文件。
	if p.APIKey != nil {
		if *p.APIKey == "" {
			if err := s.keys.Delete(APIKeyService, APIKeyAccount); err != nil && !errors.Is(err, secret.ErrNotFound) {
				return s.cur, fmt.Errorf("清除 API Key: %w", err)
			}
		} else if err := s.keys.Store(APIKeyService, APIKeyAccount, *p.APIKey); err != nil {
			if errors.Is(err, secret.ErrUnsupported) {
				return s.cur, fmt.Errorf("当前平台不支持系统凭据库，无法保存 API Key")
			}
			return s.cur, fmt.Errorf("保存 API Key: %w", err)
		}
	}

	s.cur = next
	if err := s.save(); err != nil {
		return s.cur, err
	}
	s.cur.APIKeyConfigured = s.keyConfigured()
	return s.cur, nil
}

// APIKey 读取当前保存的 key；凭据库不可用或没配时返回空串（不阻断启动）。
func (s *FileStore) APIKey() (string, error) {
	v, err := s.keys.Load(APIKeyService, APIKeyAccount)
	if errors.Is(err, secret.ErrNotFound) || errors.Is(err, secret.ErrUnsupported) {
		return "", nil
	}
	return v, err
}

func (s *FileStore) save() error {
	b, err := json.MarshalIndent(fileData{
		Theme: s.cur.Theme, FontSize: s.cur.FontSize, EditorTabSize: s.cur.EditorTabSize,
		AgentBaseURL: s.cur.AgentBaseURL, AgentModel: s.cur.AgentModel,
		AgentContextTokens: s.cur.AgentContextTokens,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化设置: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("创建设置目录: %w", err)
	}
	return os.WriteFile(s.path, append(b, 10), 0600)
}
