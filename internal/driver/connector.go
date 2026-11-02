// Package driver 定义连接器契约。
//
// 铁律：
//   - 本包只依赖 model，不认识 service / api / agent；
//   - 出站数据一律是 model 的中立结构，引擎私有类型不得越过本边界；
//   - 能力以 Capabilities() 声明，上层禁止用类型判断代替。
package driver

import (
	"context"

	"github.com/maintell/DBRover/internal/model"
)

// ConnectConfig 是建立连接所需的最小信息。
//
// User/Password 由 infranet 的凭据库注入；当前阶段直接随配置传入，
// 但绝不序列化、绝不写日志。infra/secret 完成后这两个字段会被 CredentialRef 取代。
type ConnectConfig struct {
	Name    string
	Host    string
	Port    int
	Catalog string
	Schema  string
	Params  map[string]string

	// User 是登录用户名。SQLite 不使用。
	User string
	// Password 是登录口令，仅在内存中存在。SQLite 不使用。
	Password string

	AccessMode  model.AccessMode
	Environment model.Environment
}

// HealthInfo 是连接建立后的健康与身份信息。
type HealthInfo struct {
	Version  string
	Identity model.ServerIdentity
	ReadOnly bool
	Timezone string
	Warnings []string
}

// QueryOptions 控制结果集取数行为。
type QueryOptions struct {
	// FetchSize 是单次拉取的行数，0 表示默认（200）。
	FetchSize int
	// MaxRows 是本次查询允许返回的最大行数，0 表示不限制。
	MaxRows int
}

// Cursor 是结果集游标。服务端持有，按页拉取。
type Cursor interface {
	Columns() []model.Column
	// Next 返回一批行与「是否还有更多」。读尽后返回空批与 false。
	Next(ctx context.Context) (rows [][]any, hasMore bool, err error)
	Close() error
}

// TruncationReporter 由能报告「因行数上限而被截断」的游标实现。
//
// 必须能区分「读完了」和「读够了上限」：否则调用方会把截断当成全量。
type TruncationReporter interface {
	Truncated() bool
}

// ExecResult 是不返回结果集的语句的执行结果。
type ExecResult struct {
	RowsAffected int64
	LastInsertID int64
	HasLastID    bool
	Warnings     []string
}

// Metadata 是元数据访问能力。
type Metadata interface {
	Catalogs(ctx context.Context) ([]string, error)
	Schemas(ctx context.Context, catalog string) ([]string, error)
	Objects(ctx context.Context, catalog, schema string, kinds ...model.ObjectKind) ([]model.ObjectRef, error)
	DescribeTable(ctx context.Context, ref model.ObjectRef) (*model.TableDetail, error)
	DDL(ctx context.Context, ref model.ObjectRef) (string, error)
	Search(ctx context.Context, pattern string, limit int) ([]model.ObjectRef, error)
}

// Session 是一条逻辑会话。事务进行中时，后续语句必须落在同一条物理连接上。
type Session interface {
	Query(ctx context.Context, sqlText string, args []any, opt QueryOptions) (Cursor, error)
	Execute(ctx context.Context, sqlText string, args []any) (*ExecResult, error)

	Begin(ctx context.Context) error
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
	InTx() bool

	// Cancel 中断当前在途语句。
	Cancel(ctx context.Context) error
	Close() error
}

// PoolStats 是连接池统计。
type PoolStats struct {
	Open  int
	InUse int
	Idle  int
}

// Connector 是一个引擎的连接器。
type Connector interface {
	Engine() model.Engine
	Capabilities() model.Capabilities

	Open(ctx context.Context, cfg ConnectConfig) error
	Close() error
	Health(ctx context.Context) (*HealthInfo, error)

	Meta() Metadata
	Acquire(ctx context.Context) (Session, error)
	Stats() PoolStats
}
