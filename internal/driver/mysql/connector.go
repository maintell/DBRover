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
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"

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

// classifyConnectError 把连接失败归类。
//
// 网络层交给 driver.ClassifyNetError（与引擎无关）；引擎层看错误号：
//
//	1045 口令错 / 1044 该用户无权访问这个库 → 认证
//	1049 库不存在                          → 库不存在
//
// 把这三类混成一句「连接失败」，用户只能靠猜。
func classifyConnectError(err error) driver.ConnectErrorKind {
	if k := driver.ClassifyNetError(err); k != driver.ConnectOther {
		return k
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		switch myErr.Number {
		case 1045, 1044:
			return driver.ConnectAuth
		case 1049:
			return driver.ConnectNoDatabase
		}
	}
	return driver.ConnectOther
}

// Sessions 列出当前会话（FR-MON-02）。
//
// information_schema.processlist 只列出「有权限看到」的会话：普通用户只能看到
// 自己的，PROCESS 权限才能看到全部。这是引擎的权限模型，不是我们的选择——
// 列表短不代表没有别的会话。
func (c *Connector) Sessions(ctx context.Context) ([]driver.SessionInfo, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	rows, err := h.QueryContext(ctx, `
		SELECT CAST(id AS CHAR), COALESCE(user, ''), COALESCE(host, ''), COALESCE(db, ''),
		       COALESCE(command, ''), COALESCE(info, ''), time * 1000,
		       COALESCE(command = 'Sleep', false)
		  FROM information_schema.processlist
		 WHERE id <> CONNECTION_ID()
		 ORDER BY (command = 'Sleep'), id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []driver.SessionInfo
	for rows.Next() {
		var s driver.SessionInfo
		if err := rows.Scan(&s.ID, &s.User, &s.Host, &s.Database, &s.State, &s.Query, &s.DurationMS, &s.Idle); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// KillSession 结束一条会话。
//
// KILL 只接受字面量，不能参数化，所以 id 必须先过形态校验（driver.ValidSessionID）。
// 校验放在句柄之前：形态不对时连数据库都不用碰。
func (c *Connector) KillSession(ctx context.Context, id string) error {
	if !driver.ValidSessionID(id) {
		return fmt.Errorf("mysql: 会话 id 必须是数字：%q", id)
	}
	h, err := c.handle()
	if err != nil {
		return err
	}
	if _, err := h.ExecContext(ctx, "KILL "+id); err != nil {
		return err
	}
	return nil
}

// Target 见接口注释：MySQL 的 catalog 就是库名。
func (c *Connector) Target() string { return c.config().Catalog }

// Capabilities 声明 MySQL 的能力。
func (c *Connector) Capabilities() model.Capabilities {
	return model.Capabilities{
		Transactions:      true,
		Schemas:           false, // database 即 schema，UI 只展示一层
		Views:             true,
		MaterializedViews: false,
		// 例程（过程 / 函数）走 information_schema.ROUTINES，已接进 Objects。
		Routines: true,
		// 触发器走 information_schema.TRIGGERS 单独查（见 metadata.go triggerRefs）。
		Triggers:  true,
		Sequences: false,
		UserTypes: false,
		Indexes:   true,
		Explain:   true, // EXPLAIN
		// 慢查询走 performance_schema.events_statements_summary_by_digest。
		SlowQueryStats: true,
		// 会话列表走 information_schema.processlist；终止走 KILL。
		SessionMgmt: true,
		KillSession: true,
		// 连接数与缓存命中走 SHOW GLOBAL STATUS + @@max_connections。
		Metrics: true,
		// 表体量走 information_schema.TABLES。
		Capacity:         true,
		LockGraph:        false,
		Backup:           false,
		BulkLoad:         false,
		KeysetPagination: true,
		// MySQL 的 DDL 隐式提交，无法真实回滚。
		DDLTransactional: false,
		AlterColumnType:  true,
		// ALTER COLUMN ... SET DEFAULT 与 MODIFY COLUMN ... NOT NULL 都支持
		// （后者要完整列定义，方言层已经按改类型那条路径生成）。
		AlterColumnNullability: true,
		AlterColumnDefault:     true,
		// 触发器一条语句就能建（体跟在 FOR EACH ROW 后面）。
		AddTrigger:    true,
		AddForeignKey: true,
		// 能建**过程**；建函数要求 DETERMINISTIC / READS SQL DATA / NO SQL，
		// v1 不替用户决定语义，服务层会明确拒绝并指路手写 DDL。
		AddRoutine: true,
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
		return &driver.ConnectError{Kind: classifyConnectError(err),
			Err: fmt.Errorf("mysql: 连接失败: %w", err)}
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
	db, err := c.handle()
	if err != nil {
		return nil, err
	}
	// 钉一条物理连接：取消要用 KILL QUERY <id>，而 id 只对某一条连接有意义。
	// 代价是每次取会话多一个往返（SELECT CONNECTION_ID()）——换的是
	// 「取消真的能让服务端停下来」，值得。
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	s := &session{c: c, conn: conn}
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&s.threadID); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return s, nil
}

// killQuery 用另一条连接把正在跑的那条语句杀掉。
//
// 1094（ER_NO_SUCH_THREAD）表示线程已经结束——那正是我们想要的结果，不算错误：
// 语句可能在 KILL 发出去之前刚好跑完。
func (c *Connector) killQuery(id int64) error {
	db, err := c.handle()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, fmt.Sprintf("KILL QUERY %d", id)); err != nil {
		if strings.Contains(err.Error(), "1094") || strings.Contains(err.Error(), "Unknown thread id") {
			return nil
		}
		return err
	}
	return nil
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
		// time_zone 把会话时区钉成 UTC。TIMESTAMP 列在 SELECT 时由服务端换算成
		// 会话时区，会话是 +08:00 而驱动按 loc=UTC 解析的话，取出来会整体偏 8 小时。
		// 值必须带引号并 URL 编码，驱动会原样拼成 SET time_zone='+00:00'。
		"time_zone=%27%2B00%3A00%27",
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
