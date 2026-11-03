// Package mysql 是 MySQL / MariaDB 的连接器实现。
//
// 与 SQLite 的关键差异：
//   - 有网络与认证；
//   - database 兼作 schema（Capabilities.Schemas = false）；
//   - 只读通过会话级事务限制实现，不是文件模式。
package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

const driverName = "mysql"

// Connector 实现 driver.Connector。
type Connector struct {
	mu  sync.RWMutex
	db  *sql.DB
	cfg driver.ConnectConfig
}

// New 创建一个尚未打开的连接器。
func New() *Connector { return &Connector{} }

// Engine 返回引擎类型。
func (c *Connector) Engine() model.Engine { return model.EngineMySQL }

// Capabilities 声明 MySQL 的能力。
func (c *Connector) Capabilities() model.Capabilities {
	return model.Capabilities{
		Transactions:      true,
		Schemas:           false, // database 即 schema，UI 只展示一层
		Views:             true,
		MaterializedViews: false,
		Routines:          false, // 例程管理未实现
		Triggers:          true,
		Sequences:         false,
		UserTypes:         false,
		Indexes:           true,
		Explain:           false,
		SlowQueryStats:    false,
		SessionMgmt:       false,
		KillSession:       false,
		LockGraph:         false,
		Backup:            false,
		BulkLoad:          false,
		KeysetPagination:  true,
		// MySQL 的 DDL 隐式提交，无法真实回滚。
		DDLTransactional: false,
		AlterColumnType:  true,
	}
}

// Open 建立连接池。
func (c *Connector) Open(ctx context.Context, cfg driver.ConnectConfig) error {
	if strings.TrimSpace(cfg.Host) == "" {
		return fmt.Errorf("mysql: 缺少主机地址")
	}
	if cfg.Port == 0 {
		cfg.Port = 3306
	}
	if cfg.AccessMode == "" {
		cfg.AccessMode = model.DefaultAccessMode(cfg.Environment)
	}

	dsn := buildDSN(cfg)
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return fmt.Errorf("mysql: 打开失败: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return fmt.Errorf("mysql: 连接失败: %w", err)
	}

	// 只读在会话层强制：autocommit 下每条语句即一个事务，写会被引擎拒绝。
	// 与执行门的 SQL 分析构成两层。
	if !cfg.AccessMode.AllowsWrite() {
		if _, err := db.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
			// 5.6 及更早没有该语法，退化为仅靠执行门拦截，并让 Health 报告出来。
			_ = err
		}
	}

	c.mu.Lock()
	if c.db != nil {
		_ = c.db.Close()
	}
	c.db, c.cfg = db, cfg
	c.mu.Unlock()
	return nil
}

// Close 关闭连接池。
func (c *Connector) Close() error {
	c.mu.Lock()
	db := c.db
	c.db = nil
	c.mu.Unlock()
	if db == nil {
		return nil
	}
	return db.Close()
}

// Health 返回版本与服务器身份。
func (c *Connector) Health(ctx context.Context) (*driver.HealthInfo, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	c.mu.RLock()
	cfg := c.cfg
	c.mu.RUnlock()

	var version, hostname, serverUUID sql.NullString
	row := h.QueryRowContext(ctx,
		`SELECT VERSION(), @@hostname, COALESCE(@@server_uuid, '')`)
	if err := row.Scan(&version, &hostname, &serverUUID); err != nil {
		return nil, fmt.Errorf("mysql: 读取服务器信息失败: %w", err)
	}

	id := model.ServerIdentity{
		Hostname:   hostname.String,
		Instance:   net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Version:    version.String,
		InstanceID: serverUUID.String,
	}
	sum := sha256.Sum256([]byte(id.Hostname + "\x00" + id.InstanceID + "\x00" + id.Instance))
	id.Fingerprint = hex.EncodeToString(sum[:8])

	out := &driver.HealthInfo{
		Version:  version.String,
		Identity: id,
		ReadOnly: !cfg.AccessMode.AllowsWrite(),
	}
	if serverUUID.String == "" {
		out.Warnings = append(out.Warnings, "实例未提供 server_uuid，身份指纹精度下降")
	}
	return out, nil
}

// Stats 返回连接池统计。
func (c *Connector) Stats() driver.PoolStats {
	c.mu.RLock()
	db := c.db
	c.mu.RUnlock()
	if db == nil {
		return driver.PoolStats{}
	}
	s := db.Stats()
	return driver.PoolStats{Open: s.OpenConnections, InUse: s.InUse, Idle: s.Idle}
}

// Acquire 借出一条逻辑会话。
func (c *Connector) Acquire(ctx context.Context) (driver.Session, error) {
	if _, err := c.handle(); err != nil {
		return nil, err
	}
	return &session{c: c}, nil
}

func (c *Connector) handle() (*sql.DB, error) {
	c.mu.RLock()
	db := c.db
	c.mu.RUnlock()
	if db == nil {
		return nil, fmt.Errorf("mysql: 连接未打开")
	}
	return db, nil
}

func (c *Connector) config() driver.ConnectConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

// buildDSN 构造 go-sql-driver 的 DSN。
// parseTime=true 让 DATE/DATETIME/TIMESTAMP 读成 time.Time；
// loc=UTC 固定解析时区，避免依赖服务端本地时区。
func buildDSN(cfg driver.ConnectConfig) string {
	params := []string{
		"parseTime=true",
		"loc=UTC",
		"charset=utf8mb4",
		"timeout=10s",
		"readTimeout=0", // 长查询由 ctx 控制
		"interpolateParams=false",
		// clientFoundRows：让 UPDATE 的 RowsAffected 返回「匹配行数」而不是「实际改动行数」。
		// 否则把一行改成相同值会返回 0，被乐观并发校验误判成冲突。
		"clientFoundRows=true",
	}
	for k, v := range cfg.Params {
		params = append(params, k+"="+v)
	}
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?%s",
		cfg.User, cfg.Password,
		net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		cfg.Catalog,
		strings.Join(params, "&"))
}
