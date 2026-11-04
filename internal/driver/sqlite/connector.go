// Package sqlite 是 SQLite 的连接器实现（纯 Go，免 CGO）。
package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

const driverName = "sqlite"

// Connector 实现 driver.Connector。
type Connector struct {
	mu   sync.RWMutex
	db   *sql.DB
	path string
	cfg  driver.ConnectConfig
}

// New 创建一个尚未打开的连接器。
func New() *Connector { return &Connector{} }

// Engine 返回引擎类型。
func (c *Connector) Engine() model.Engine { return model.EngineSQLite }

// Capabilities 声明 SQLite 的能力。
//
// 铁律：只有「实现 + 有对外入口 + 有测试」的能力才能标 true。
// 标 true 而无人消费，会让界面开出点不动的按钮。
func (c *Connector) Capabilities() model.Capabilities {
	return model.Capabilities{
		Transactions:      true,
		Schemas:           false, // 无 schema 层，只有 main/temp/attached 的 catalog
		Views:             true,
		MaterializedViews: false,
		Routines:          false,
		Triggers:          true,
		Sequences:         false,
		UserTypes:         false,
		Indexes:           true,
		Explain:           true, // EXPLAIN QUERY PLAN
		SlowQueryStats:    false,
		SessionMgmt:       false,
		KillSession:       false,
		LockGraph:         false,
		Backup:            false, // 备份/恢复尚未实现
		BulkLoad:          false,
		KeysetPagination:  true,
		DDLTransactional:  true,
		// SQLite 不能改列类型、可空性与默认值——都只能重建表。
		AlterColumnType:        false,
		AlterColumnNullability: false,
		AlterColumnDefault:     false,
		// SQLite 的触发器件是 BEGIN … END，一条语句，切分器已经认它。
		AddTrigger:    true,
		AddForeignKey: false,
		// SQLite 没有存储过程与函数，例程这条路整个不通。
		AddRoutine: false,
	}
}

// Open 打开数据库文件。只读连接会同时使用 mode=ro 与 PRAGMA query_only，
// 形成协议层与分析层之外的第二道保护。
func (c *Connector) Open(ctx context.Context, cfg driver.ConnectConfig) error {
	path := strings.TrimSpace(cfg.Host)
	if path == "" {
		return fmt.Errorf("sqlite: 连接需要文件路径")
	}
	if cfg.AccessMode == "" {
		cfg.AccessMode = model.DefaultAccessMode(cfg.Environment)
	}
	// 未知访问模式按只读处理：失败关闭，不猜「大概是想读写」。
	ro := !cfg.AccessMode.AllowsWrite()
	memory := path == ":memory:" || strings.HasPrefix(path, "file::memory:")

	if !memory && ro {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("sqlite: 只读连接要求文件已存在: %w", err)
		}
	}

	db, err := sql.Open(driverName, dsnFor(path, memory, ro))
	if err != nil {
		return fmt.Errorf("sqlite: 打开失败: %w", err)
	}
	if memory {
		// 内存库每个连接是独立的库，必须限制为单连接。
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(4)
	}
	db.SetMaxIdleConns(2)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("sqlite: 连接失败: %w", err)
	}
	if ro {
		if _, err := db.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
			_ = db.Close()
			return fmt.Errorf("sqlite: 设置只读失败: %w", err)
		}
	}

	c.mu.Lock()
	if c.db != nil {
		_ = c.db.Close()
	}
	c.db, c.path, c.cfg = db, path, cfg
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

// Health 返回版本与服务器身份。身份指纹用于检测「同连接串连到不同库」。
func (c *Connector) Health(ctx context.Context) (*driver.HealthInfo, error) {
	c.mu.RLock()
	db, cfg, path := c.db, c.cfg, c.path
	c.mu.RUnlock()
	if db == nil {
		return nil, fmt.Errorf("sqlite: 连接未打开")
	}

	var version string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		return nil, fmt.Errorf("sqlite: 读取版本失败: %w", err)
	}

	id := model.ServerIdentity{
		Hostname: filepath.Base(path),
		Instance: path,
		Version:  version,
	}
	id.Fingerprint = fingerprint(id)

	h := &driver.HealthInfo{
		Version:  version,
		Identity: id,
		ReadOnly: !cfg.AccessMode.AllowsWrite(),
	}
	if path == ":memory:" {
		h.Warnings = append(h.Warnings, "内存数据库：进程退出后数据丢失")
	}
	return h, nil
}

// Stats 返回连接池统计。
func (c *Connector) Stats() driver.PoolStats {
	c.mu.RLock()
	db := c.db
	c.mu.RUnlock()
	if db == nil {
		return driver.PoolStats{}
	}
	return driver.PoolStats{Open: db.Stats().OpenConnections, InUse: db.Stats().InUse, Idle: db.Stats().Idle}
}

// Acquire 借出一条逻辑会话。
func (c *Connector) Acquire(ctx context.Context) (driver.Session, error) {
	c.mu.RLock()
	db := c.db
	c.mu.RUnlock()
	if db == nil {
		return nil, fmt.Errorf("sqlite: 连接未打开")
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("sqlite: 会话不可用: %w", err)
	}
	return &session{c: c}, nil
}

func fingerprint(id model.ServerIdentity) string {
	sum := sha256.Sum256([]byte(id.Hostname + "\x00" + id.Instance + "\x00" + id.Version))
	return hex.EncodeToString(sum[:8])
}

// dsnFor 构造 modernc/sqlite 的 DSN。
// _pragma 会在每条新连接上执行，因此 busy_timeout 与外键约束是全局生效的。
func dsnFor(path string, memory, ro bool) string {
	if memory {
		return "file::memory:?cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "file:") {
		p = "file:" + p
	}
	sep := "?"
	if strings.Contains(p, "?") {
		sep = "&"
	}
	q := sep + "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if ro {
		q += "&mode=ro"
	}
	return p + q
}
