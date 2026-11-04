// Package pg 是 PostgreSQL 的连接器实现。
//
// 与 MySQL 的关键差异：
//   - 真正有两层命名空间：catalog（database）+ schema；
//   - DDL 没有一键获取接口，需要从系统目录拼装；
//   - 只读可用 SET default_transaction_read_only 在会话层强制；
//   - DDL 是事务性的，可真实回滚。
package pg

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

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

const driverName = "pgx"

// Connector 实现 driver.Connector。
type Connector struct {
	mu  sync.RWMutex
	db  *sql.DB
	cfg driver.ConnectConfig
}

// New 创建一个尚未打开的连接器。
func New() *Connector { return &Connector{} }

// Engine 返回引擎类型。
func (c *Connector) Engine() model.Engine { return model.EnginePostgres }

// Capabilities 声明 PostgreSQL 的能力。
func (c *Connector) Capabilities() model.Capabilities {
	return model.Capabilities{
		Transactions:      true,
		Schemas:           true, // catalog + schema 两层真实存在
		Views:             true,
		MaterializedViews: true,
		Routines:          false,
		// 同 MySQL：pg_trigger 还没查，声明 true 会造成「没有触发器」的错觉。
		Triggers:         false,
		Sequences:        true,
		UserTypes:        true,
		Indexes:          true,
		Explain:          true, // EXPLAIN
		SlowQueryStats:   false,
		SessionMgmt:      false,
		KillSession:      false,
		LockGraph:        false,
		Backup:           false,
		BulkLoad:         false,
		KeysetPagination: true,
		// PG 的 DDL 在事务内，可真实回滚。
		DDLTransactional: true,
		AlterColumnType:  true,
		AddForeignKey:    true,
	}
}

// Open 建立连接池。
func (c *Connector) Open(ctx context.Context, cfg driver.ConnectConfig) error {
	if strings.TrimSpace(cfg.Host) == "" {
		return fmt.Errorf("postgres: 缺少主机地址")
	}
	if cfg.Port == 0 {
		cfg.Port = 5432
	}
	if cfg.AccessMode == "" {
		cfg.AccessMode = model.DefaultAccessMode(cfg.Environment)
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	// 只读在最底层强制：写进**连接参数**而不是开好池再 SET 一次——
	// sql.DB 是连接池，那条 SET 只作用于恰好被选中的那一条物理连接，
	// 池里其余连接仍然是可写的。
	if !cfg.AccessMode.AllowsWrite() {
		cfg.Params["default_transaction_read_only"] = "on"
	}
	// 会话时区固定 UTC：timestamptz 的换算与 timestamp 字面量的解释都以它为基准。
	// 与展示无关——展示在返回行时按 DisplayTimeZone 转换。
	if _, ok := cfg.Params["timezone"]; !ok {
		cfg.Params["timezone"] = "UTC"
	}

	db, err := sql.Open(driverName, buildDSN(cfg))
	if err != nil {
		return fmt.Errorf("postgres: 打开失败: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return fmt.Errorf("postgres: 连接失败: %w", err)
	}

	if cfg.Schema == "" {
		cfg.Schema = "public"
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

	var (
		version   string
		addr      sql.NullString
		database  string
		serverVer string
		sysID     sql.NullString
	)
	row := h.QueryRowContext(ctx, `
		SELECT version(),
		       COALESCE(inet_server_addr()::text,''),
		       current_database(),
		       current_setting('server_version'),
		       COALESCE((SELECT system_identifier::text FROM pg_control_system()),'')`)
	if err := row.Scan(&version, &addr, &database, &serverVer, &sysID); err != nil {
		return nil, fmt.Errorf("postgres: 读取服务器信息失败: %w", err)
	}

	id := model.ServerIdentity{
		Hostname:   addr.String,
		Instance:   database,
		Version:    serverVer,
		InstanceID: sysID.String,
	}
	sum := sha256.Sum256([]byte(id.Hostname + "\x00" + id.InstanceID + "\x00" + id.Instance))
	id.Fingerprint = hex.EncodeToString(sum[:8])

	out := &driver.HealthInfo{
		Version:  serverVer,
		Identity: id,
		ReadOnly: !cfg.AccessMode.AllowsWrite(),
	}
	if sysID.String == "" {
		out.Warnings = append(out.Warnings, "无法读取 system_identifier（需要 pg_control_system 权限），身份指纹精度下降")
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
		return nil, fmt.Errorf("postgres: 连接未打开")
	}
	return db, nil
}

func (c *Connector) config() driver.ConnectConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

func buildDSN(cfg driver.ConnectConfig) string {
	db := cfg.Catalog
	if db == "" {
		db = "postgres"
	}
	sslmode := "disable"
	params := []string{"sslmode=" + sslmode, "connect_timeout=10"}
	for k, v := range cfg.Params {
		if k == "sslmode" {
			params[0] = "sslmode=" + v
			continue
		}
		params = append(params, k+"="+v)
	}
	return fmt.Sprintf("postgres://%s:%s@%s/%s?%s",
		cfg.User, cfg.Password,
		net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		db,
		strings.Join(params, "&"))
}
