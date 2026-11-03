// Package driver 定义连接器契约。
//
// 铁律：
//   - 本包只依赖 model，不认识 service / api / agent；
//   - 出站数据一律是 model 的中立结构，引擎私有类型不得越过本边界；
//   - 能力以 Capabilities() 声明，上层禁止用类型判断代替。
package driver

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/maintell/DBRover/internal/model"
)

// ErrObjectNotFound 表示目标对象不存在。
//
// Describe* 系列必须返回它（或包装它），**不得**返回一个零列的空结构：
// PRAGMA table_info 与 information_schema 对不存在的表都只是「查不到行」，
// 一不小心就会把「表不存在」说成「表没有列」，上层据此会给出误导性提示
// （例如把建表失败说成「没有主键，无法编辑」）。
var ErrObjectNotFound = errors.New("对象不存在")

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

	// DisplayTimeZone 是时间值返回给上层时使用的时区（IANA 名，如 Asia/Shanghai）。
	// 空表示 UTC——**不是**「跟随服务器本地时区」：跨引擎比对与日志里
	// 一个固定基准比「看起来是本机时间」重要得多（docs/06 §1.4）。
	// 只影响展示，不影响存储：驱动会把握手时区固定为 UTC，写进去的就是 UTC。
	DisplayTimeZone string
}

// ViewReferencesTable 判断一段视图定义是否引用了某张表。
//
// 这是给「引擎拿不到精确依赖关系」的驱动器用的兜底（SQLite、MySQL 5.7），
// 结果必须标 Dependent.Exact=false。匹配用词边界且大小写不敏感：宁可多报
// 一次人工确认，也不要漏掉一个真的引用——删表没有回滚。
func ViewReferencesTable(viewSQL, table string) bool {
	if strings.TrimSpace(viewSQL) == "" || strings.TrimSpace(table) == "" {
		return false
	}
	// 表名里的正则元字符先转义，再用词边界包起来。
	pat := `(?i)\b` + regexp.QuoteMeta(table) + `\b`
	re, err := regexp.Compile(pat)
	if err != nil {
		return false
	}
	return re.MatchString(viewSQL)
}

// 空值或非法名一律退回 UTC：宁可显示成 UTC，也不要按服务器本地时区猜——
// 猜错的时区会让人对着差 8 小时的数据排查半天。
func (c ConnectConfig) DisplayLocation() *time.Location {
	name := c.DisplayTimeZone
	if name == "" || name == "UTC" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// ConvertTimeValues 把一行里的 time.Time 换算到目标时区（原地修改）。
//
// 放在 driver 层是因为所有读路径（表浏览、查询分页、导出、游标续页）
// 都经过这两个驱动的 cursor，这是唯一一个能一次改对的地方。
// 只动 time.Time：DATE 也被驱动读成 time.Time，换算后仍是同一天（除非
// 目标时区跨零点的极端情形），比让它们各自为政更一致。
func ConvertTimeValues(vals []any, loc *time.Location) {
	if loc == nil {
		return
	}
	for i, v := range vals {
		if t, ok := v.(time.Time); ok {
			vals[i] = t.In(loc)
		}
	}
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
	// Dependents 返回依赖 ref 的其他对象（入向外键、引用它的视图）。
	//
	// 删除前的阻断靠它（docs/07 §5.1 / §AD-0006）。实现必须「能精确就精确」：
	// 入向外键三个引擎都有精确来源，视图依赖可能只能文本匹配——后者要带
	// Exact=false，让提示文案能如实标注「可能误报」。
	// ref 不存在时返回空列表而不是错误：删除一个已经被别人删掉的对象，
	// 不该因为依赖查询失败而变成一个看不懂的报错。
	Dependents(ctx context.Context, ref model.ObjectRef) ([]model.Dependent, error)
	// Blockers 返回可能让 DDL 卡住的会话（长事务、以及已经在等锁的会话）。
	//
	// threshold 是「多久算长」。实现要分清两种性质，调用方据此决定提示还是阻断：
	// idle_in_transaction 在忙碌的库上永远存在，只该提示；lock_wait 说明已经
	// 有人在等锁，再上一条拿锁的 DDL 只会让队列更长。
	// 不支持会话观察的引擎返回空列表即可，不要返回错误。
	Blockers(ctx context.Context, threshold time.Duration) ([]model.Blocker, error)
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
