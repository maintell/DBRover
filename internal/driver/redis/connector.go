package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

// Connector 实现 driver.KVConnector。
//
// 它**不实现** driver.Connector：Redis 没有表、列、SQL，硬套会得到一堆永远
// 返空的方法（ADR-0007）。
type Connector struct {
	mu  sync.RWMutex
	cli *redis.Client
	cfg driver.ConnectConfig
	// cluster 为 true 表示连的是集群；第一版不验证集群行为，只在 Health 里告警。
	cluster bool
}

// New 创建一个尚未打开的连接器。
func New() *Connector { return &Connector{} }

func (c *Connector) Engine() model.Engine { return model.EngineRedis }

// Capabilities 声明 Redis 的能力。
func (c *Connector) Capabilities() model.Capabilities {
	return model.Capabilities{
		Transactions: false, // MULTI/EXEC 只是排队，出错不回滚，不当作事务
		Schemas:      false,
		Views:        false,
		Indexes:      false,
		Explain:      false,
		Backup:       false,
		BulkLoad:     false,
		// 键集分页不适用：Redis 只有 SCAN 游标。
		KeysetPagination: false,
		DDLTransactional: false,
		AlterColumnType:  false,
		AddForeignKey:    false,
	}
}

// Open 建立连接。
//
// 只读模式在**连接层**强制（docs/04 §3.1：Redis 没有服务端只读开关），
// 具体做法是所有写命令在 Mutate / Command 里被拒绝——这不是装饰：
// 只读连接上执行 SET 必须在发出前就拦住，而不是指望 Redis 拒绝。
func (c *Connector) Open(ctx context.Context, cfg driver.ConnectConfig) error {
	if strings.TrimSpace(cfg.Host) == "" {
		return fmt.Errorf("redis: 缺少主机地址")
	}
	if cfg.Port == 0 {
		cfg.Port = 6379
	}
	if cfg.AccessMode == "" {
		cfg.AccessMode = model.DefaultAccessMode(cfg.Environment)
	}
	db := 0
	if cfg.Catalog != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cfg.Catalog))
		if err != nil || n < 0 {
			return fmt.Errorf("redis: catalog 应是逻辑库编号（0-15），实际 %q", cfg.Catalog)
		}
		db = n
	}

	cli := redis.NewClient(&redis.Options{
		Addr:        cfg.Host + ":" + strconv.Itoa(cfg.Port),
		Username:    cfg.User,
		Password:    cfg.Password,
		DB:          db,
		DialTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second,
		// 写超时给长一点：SCAN 大 keyspace 时会一直读，不写。
		WriteTimeout: 10 * time.Second,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := cli.Ping(pingCtx).Err(); err != nil {
		_ = cli.Close()
		return fmt.Errorf("redis: 连接失败: %w", err)
	}

	// 探测集群：集群模式下多 key 命令需要同 slot，本工具第一版不验证。
	cluster := false
	if info, err := cli.ClusterInfo(pingCtx).Result(); err == nil {
		cluster = strings.Contains(info, "cluster_enabled:1")
	}

	c.mu.Lock()
	if c.cli != nil {
		_ = c.cli.Close()
	}
	c.cli, c.cfg, c.cluster = cli, cfg, cluster
	c.mu.Unlock()
	return nil
}

func (c *Connector) Close() error {
	c.mu.Lock()
	cli := c.cli
	c.cli = nil
	c.mu.Unlock()
	if cli == nil {
		return nil
	}
	return cli.Close()
}

// Health 返回版本、只读状态与身份指纹。
func (c *Connector) Health(ctx context.Context) (*driver.HealthInfo, error) {
	cli, err := c.handle()
	if err != nil {
		return nil, err
	}
	info, err := cli.Info(ctx, "server").Result()
	if err != nil {
		return nil, fmt.Errorf("redis: 读取服务器信息失败: %w", err)
	}
	version := parseInfoField(info, "redis_version")
	mode := parseInfoField(info, "redis_mode")

	identity := model.ServerIdentity{Version: version, Instance: "redis-" + mode}
	if runID := parseInfoField(info, "run_id"); runID != "" {
		identity.InstanceID = runID
	}
	sum := sha256.Sum256([]byte(identity.InstanceID + "\x00" + version))
	identity.Fingerprint = hex.EncodeToString(sum[:8])

	out := &driver.HealthInfo{
		Version:  version,
		Identity: identity,
		ReadOnly: !c.config().AccessMode.AllowsWrite(),
	}
	if c.clusterMode() {
		out.Warnings = append(out.Warnings,
			"检测到集群模式：本工具第一版未验证集群行为（多 key 命令需同 slot、SCAN 需逐节点），连接可用但结果可能不完整")
	}
	return out, nil
}

func (c *Connector) handle() (*redis.Client, error) {
	c.mu.RLock()
	cli := c.cli
	c.mu.RUnlock()
	if cli == nil {
		return nil, fmt.Errorf("redis: 连接未打开")
	}
	return cli, nil
}

func (c *Connector) config() driver.ConnectConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

func (c *Connector) clusterMode() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cluster
}

func (c *Connector) writable() bool { return c.config().AccessMode.AllowsWrite() }

// parseInfoField 从 INFO 的 key:value 行里取一个字段。
func parseInfoField(info, key string) string {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+":"))
		}
	}
	return ""
}
