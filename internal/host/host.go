package host

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/maintell/DBRover/internal/api"
	"github.com/maintell/DBRover/internal/infra/settings"
	"github.com/maintell/DBRover/internal/service"
)

// Options 是一条进程的可选装配项。零值 = 桌面默认形态。
type Options struct {
	// Token 为空时 api.New 会随机生成（每次启动不重样）。
	Token        string
	AuditPath    string
	AnchorPath   string
	MetricsPath  string
	SettingsPath string
}

// Runtime 是一次进程生命周期内所有需要关闭的资源。
type Runtime struct {
	Server      *api.Server
	Settings    settings.Store
	AgentConfig api.AgentConfig
	closers     []func() error
	closed      bool
}

// Close 关掉连接、审计、指标留存。重复调用安全。
func (rt *Runtime) Close() {
	if rt.closed {
		return
	}
	rt.closed = true
	rt.Server.Close()
	for i := len(rt.closers) - 1; i >= 0; i-- {
		_ = rt.closers[i]()
	}
}

// New 装配一条进程所需的全部依赖。serve 与桌面都调它，保证两种形态一致。
func New(o Options) (*Runtime, error) {
	var st settings.Store
	fs, err := settings.NewFileStore(o.SettingsPath)
	if err != nil {
		// NewFileStore 只在极端情况返回错误；退到内存默认设置，不让应用起不来。
		fmt.Fprintf(os.Stderr, "设置不可用，改用内存默认设置（本次不持久化）：%v\n", err)
		st = settings.NewMemStore(settings.Defaults())
	} else {
		st = fs
	}

	audit, err := service.NewAuditLog(o.AuditPath, 0)
	if err != nil {
		return nil, fmt.Errorf("打开审计日志: %w", err)
	}
	rt := &Runtime{}
	rt.closers = append(rt.closers, audit.Close)

	if o.AnchorPath != "" {
		key, err := AnchorKey()
		if err != nil {
			_ = audit.Close()
			return nil, err
		}
		a, err := service.OpenAnchor(o.AnchorPath, key)
		if err != nil {
			_ = audit.Close()
			return nil, err
		}
		audit.SetAnchor(a)
		fmt.Fprintf(os.Stderr, "审计锚点: %s（每 %d 条封存一次链头）\n", o.AnchorPath, 100)
	}

	s := api.New(o.Token, OpenConnector, audit)
	rt.Server = s
	rt.Settings = st

	// 设置变化时（主要是模型服务与 API Key）重建 Agent 配置，作用于下一次会话。
	s.UseSettings(st, func() {
		s.SetAgentConfig(AgentConfig(st))
	})

	if o.MetricsPath != "" {
		mst, err := service.OpenMetricStore(o.MetricsPath)
		if err != nil {
			rt.Close()
			return nil, fmt.Errorf("打开指标留存: %w", err)
		}
		rt.closers = append(rt.closers, mst.Close)
		s.UseMetricStore(mst)
		fmt.Fprintf(os.Stderr, "指标留存: %s（面板打开时采样，保留 7 天）\n", o.MetricsPath)
	}

	rt.AgentConfig = AgentConfig(st)
	s.SetAgentConfig(rt.AgentConfig)
	return rt, nil
}

// AgentConfig 装配模型服务配置。
//
// 优先级：环境变量显式设置（CI / 临时调试）覆盖持久化设置；API Key 取
// 「环境变量 → 设置面板写入的凭据库」。没有配置时返回零值，Agent 端点会明确拒绝。
func AgentConfig(st settings.Store) api.AgentConfig {
	cfg := api.AgentConfig{}
	if s, err := st.Load(); err == nil {
		cfg.BaseURL = s.AgentBaseURL
		cfg.Model = s.AgentModel
		cfg.ContextTokens = s.AgentContextTokens
	}
	if v := strings.TrimSpace(os.Getenv("DBROVER_AGENT_BASE_URL")); v != "" {
		cfg.BaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("DBROVER_AGENT_MODEL")); v != "" {
		cfg.Model = v
	}
	if v := strings.TrimSpace(os.Getenv("DBROVER_AGENT_CONTEXT_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ContextTokens = n
		}
	}
	if v := os.Getenv("DBROVER_AGENT_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if cfg.APIKey == "" {
		if k, err := st.APIKey(); err == nil {
			cfg.APIKey = k
		}
	}
	return cfg
}
