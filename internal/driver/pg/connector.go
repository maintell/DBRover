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
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

// classifyConnectError 把连接失败归类。
//
// 网络层交给 driver.ClassifyNetError（与引擎无关）；引擎层看 SQLSTATE，
// 因为把口令错说成「连接失败」几乎等于没说：
//
//	28P01 invalid_password / 28000 invalid_authorization → 认证
//	3D000 invalid_catalog_name                        → 库不存在
func classifyConnectError(err error) driver.ConnectErrorKind {
	if k := driver.ClassifyNetError(err); k != driver.ConnectOther {
		return k
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01", "28000":
			return driver.ConnectAuth
		case "3D000":
			return driver.ConnectNoDatabase
		}
	}
	return driver.ConnectOther
}

// Sessions 列出当前会话（FR-MON-02）。
//
// pg_stat_activity 是**集群级**的：能看到所有库的会话，这正合运维的用法——
// 出问题时通常不是只看自己这个库。排除自己这条，否则列表第一行永远是自己。
func (c *Connector) Sessions(ctx context.Context) ([]driver.SessionInfo, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	rows, err := h.QueryContext(ctx, `
		SELECT pid::text,
		       COALESCE(usename, ''),
		       COALESCE(host(client_addr), ''),
		       COALESCE(datname, ''),
		       COALESCE(state, ''),
		       COALESCE(query, ''),
		       COALESCE(EXTRACT(EPOCH FROM (now() - COALESCE(query_start, state_change, backend_start))) * 1000, 0)::bigint,
		       COALESCE(state = 'idle', false)
		  FROM pg_stat_activity
		 WHERE pid <> pg_backend_pid()
		   AND backend_type = 'client backend'
		 ORDER BY (state = 'idle'), pid`)
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
// id 必须自己校验：数字之外一律拒绝。pg_terminate_backend 接受参数，但 id 是
// 调用方给的字符串，先卡形态比「让 PG 报个类型错」更早也更清楚。
func (c *Connector) KillSession(ctx context.Context, id string) error {
	if !driver.ValidSessionID(id) {
		return fmt.Errorf("postgres: 会话 id 必须是数字：%q", id)
	}
	h, err := c.handle()
	if err != nil {
		return err
	}
	var ok bool
	if err := h.QueryRowContext(ctx, "SELECT pg_terminate_backend($1::int)", id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("postgres: 结束会话 %s 未成功（可能已经断开）", id)
	}
	return nil
}

// Target 见接口注释：PostgreSQL 的 catalog 就是库名。
func (c *Connector) Target() string { return c.config().Catalog }

// Capabilities 声明 PostgreSQL 的能力。
func (c *Connector) Capabilities() model.Capabilities {
	return model.Capabilities{
		Transactions:      true,
		Schemas:           true, // catalog + schema 两层真实存在
		Views:             true,
		MaterializedViews: true,
		// 例程走 pg_proc（prokind 为 f/p），已接进 Objects；扩展自带的函数不算
		// 「你的对象」，靠 pg_depend 的 deptype='e' 排除。
		Routines: true,
		// 触发器走 pg_trigger 单独查，已排除 tgisinternal（外键的内部触发器）。
		Triggers: true,
		// 序列就是 pg_class 里 relkind='S' 的对象，已接进 Objects。
		Sequences: true,
		// 用户类型（枚举 / 域）走 pg_type，已接进 Objects。
		UserTypes: true,
		Indexes:   true,
		// ALTER TABLE ... ALTER COLUMN 能直接改可空性与默认值。
		AlterColumnNullability: true,
		AlterColumnDefault:     true,
		// CHECK 约束真会被强制（真机验证：插入违反约束的行被拒）。
		AddCheck: true,
		// indisvalid 查得到：无效索引（失败的 CREATE INDEX CONCURRENTLY 会留下）
		// 照样出现在清单里，只看「有几个索引」发现不了。
		IndexValidity: true,
		Explain:       true, // EXPLAIN
		// 慢查询走 pg_stat_statements（没开时降级并给出打开步骤）。
		SlowQueryStats: true,
		// 会话列表走 pg_stat_activity；终止走 pg_terminate_backend。
		SessionMgmt: true,
		KillSession: true,
		// 连接数与缓存命中走 pg_stat_database + pg_settings。
		Metrics: true,
		// 表体量走 pg_total_relation_size / pg_indexes_size（目录计算，不扫表）。
		Capacity:         true,
		LockGraph:        false,
		Backup:           false,
		BulkLoad:         false,
		KeysetPagination: true,
		// PG 的 DDL 在事务内，可真实回滚。
		DDLTransactional: true,
		AlterColumnType:  true,
		AddForeignKey:    true,
		// 过程与函数都能建：函数要返回类型，体放进 dollar-quote。
		AddRoutine: true,
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
		return &driver.ConnectError{Kind: classifyConnectError(err),
			Err: fmt.Errorf("postgres: 连接失败: %w", err)}
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
