// Package model 定义跨引擎共享的领域模型。
//
// 铁律：本包不得 import 任何其它 internal 包，也不得出现引擎私有类型。
// 引擎差异一律通过 Capabilities 与 raw 字段表达。
package model

import (
	"strconv"
	"strings"
)

// Engine 是数据库引擎类型。
type Engine string

const (
	EnginePostgres      Engine = "postgres"
	EngineMySQL         Engine = "mysql"
	EngineMariaDB       Engine = "mariadb"
	EngineMSSQL         Engine = "mssql"
	EngineOracle        Engine = "oracle"
	EngineSQLite        Engine = "sqlite"
	EngineRedis         Engine = "redis"
	EngineMongoDB       Engine = "mongodb"
	EngineElasticsearch Engine = "elasticsearch"
)

// AllEngines 按 UI 展示顺序返回全部引擎。
func AllEngines() []Engine {
	return []Engine{
		EnginePostgres, EngineMySQL, EngineMariaDB, EngineMSSQL, EngineOracle,
		EngineSQLite, EngineRedis, EngineMongoDB, EngineElasticsearch,
	}
}

// IsSQL 报告该引擎是否使用 SQL 方言（含命令模式的关系库）。
func (e Engine) IsSQL() bool {
	switch e {
	case EnginePostgres, EngineMySQL, EngineMariaDB, EngineMSSQL, EngineOracle, EngineSQLite:
		return true
	default:
		return false
	}
}

// IsRelational 报告该引擎是否具备表/列/索引等关系结构。
func (e Engine) IsRelational() bool {
	switch e {
	case EnginePostgres, EngineMySQL, EngineMariaDB, EngineMSSQL, EngineOracle, EngineSQLite:
		return true
	default:
		return false
	}
}

// Environment 是连接的目标环境。它与 AccessMode 正交：
// 前者描述「这是什么库」，后者描述「允许做到什么程度」。
type Environment string

const (
	EnvLocal      Environment = "local"
	EnvTest       Environment = "test"
	EnvStaging    Environment = "staging"
	EnvProduction Environment = "production"
	EnvUnknown    Environment = "unknown"
)

// Strictness 返回严格度：0 最松，3 最严。
//
// unknown 的严格度等同 production —— 默认值的代价必须落在「麻烦」一侧。
func (e Environment) Strictness() int {
	switch e {
	case EnvLocal:
		return 0
	case EnvTest:
		return 1
	case EnvStaging:
		return 2
	case EnvProduction, EnvUnknown:
		return 3
	default:
		return 3
	}
}

// IsProductionLike 报告是否按生产严格度处理。
func (e Environment) IsProductionLike() bool { return e.Strictness() >= 3 }

// CanRelax 报告从当前环境放宽到 target 是否需要人工确认。
// 只有严格度下降才算放宽。
func (e Environment) CanRelax(to Environment) bool { return to.Strictness() < e.Strictness() }

// ParseEnvironment 解析环境字符串，非法值回落为 unknown。
func ParseEnvironment(s string) Environment {
	switch Environment(strings.ToLower(strings.TrimSpace(s))) {
	case EnvLocal:
		return EnvLocal
	case EnvTest:
		return EnvTest
	case EnvStaging:
		return EnvStaging
	case EnvProduction:
		return EnvProduction
	default:
		return EnvUnknown
	}
}

// AccessMode 是连接级权限声明。
type AccessMode string

const (
	AccessReadOnly  AccessMode = "read-only"
	AccessReadWrite AccessMode = "read-write"
	AccessAdmin     AccessMode = "admin"
)

// AllowsWrite 报告该访问模式是否允许写操作。
//
// 注意：空值与未知值一律视为不可写 —— 调用方必须先经过 ParseAccessMode 校验，
// 不能把用户输入直接转成 AccessMode 再比较。
func (a AccessMode) AllowsWrite() bool { return a == AccessReadWrite || a == AccessAdmin }

// ParseAccessMode 严格解析访问模式。空字符串表示「未指定」，由调用方套默认值。
// 拼错的值必须被拒绝：`read_only` 这种写法如果被当成未知模式放行，
// 会让只读守卫与驱动的只读模式同时失效。
func ParseAccessMode(s string) (AccessMode, bool) {
	switch AccessMode(strings.ToLower(strings.TrimSpace(s))) {
	case "":
		return "", true
	case AccessReadOnly:
		return AccessReadOnly, true
	case AccessReadWrite:
		return AccessReadWrite, true
	case AccessAdmin:
		return AccessAdmin, true
	default:
		return AccessReadOnly, false
	}
}

// ObjectKind 是对象类型。
type ObjectKind string

const (
	KindTable      ObjectKind = "table"
	KindView       ObjectKind = "view"
	KindMatView    ObjectKind = "matview"
	KindRoutine    ObjectKind = "routine"
	KindSequence   ObjectKind = "sequence"
	KindType       ObjectKind = "type"
	KindTrigger    ObjectKind = "trigger"
	KindIndex      ObjectKind = "index"
	KindCollection ObjectKind = "collection"
)

// TypeKind 是中立类型分类，用于跨引擎映射与 UI 渲染。
type TypeKind string

const (
	TypeUnknown   TypeKind = "unknown"
	TypeInteger   TypeKind = "integer"
	TypeDecimal   TypeKind = "decimal"
	TypeFloat     TypeKind = "float"
	TypeString    TypeKind = "string"
	TypeText      TypeKind = "text"
	TypeBinary    TypeKind = "binary"
	TypeBoolean   TypeKind = "boolean"
	TypeDate      TypeKind = "date"
	TypeTime      TypeKind = "time"
	TypeTimestamp TypeKind = "timestamp"
	TypeInterval  TypeKind = "interval"
	TypeUUID      TypeKind = "uuid"
	TypeJSON      TypeKind = "json"
	TypeArray     TypeKind = "array"
	TypeEnum      TypeKind = "enum"
	TypeGeometry  TypeKind = "geometry"
	TypeNetwork   TypeKind = "network"
)

// ColumnType 是中立列类型。Raw 保留引擎原始类型串，
// 因为精确还原比统一更重要（迁移时优先用 Raw 映射）。
type ColumnType struct {
	Kind      TypeKind    `json:"kind"`
	Raw       string      `json:"raw,omitempty"`
	Bits      int         `json:"bits,omitempty"`
	Precision int         `json:"precision,omitempty"`
	Scale     int         `json:"scale,omitempty"`
	MaxLen    int         `json:"max_len,omitempty"`
	Fixed     bool        `json:"fixed,omitempty"`
	WithTZ    bool        `json:"with_tz,omitempty"`
	Values    []string    `json:"values,omitempty"`
	Elem      *ColumnType `json:"elem,omitempty"`
}

// TypeArgs 从类型声明里取出括号内的两个整数（"varchar(64)" → 64, 0；
// "decimal(10,2)" → 10, 2）。
//
// 只解析**第一组**括号，且只认整数：这是各引擎类型声明里唯一通用的形状。
// ok 为 false 表示没有括号参数（text / json / integer），调用方必须把它当成
// 「不知道」，而不是「上限是 0」——把 0 当上限会把一切安全情况判成丢数据。
//
// 为什么放在 model：三个驱动的 NeutralType 与迁移预检都要用它，各写一份就会
// 出现「MySQL 认 varchar(64)、PG 只认 numeric(10,2)」这种悄悄不一致。
func TypeArgs(raw string) (first, second int, ok bool) {
	i := strings.IndexByte(raw, '(')
	if i < 0 {
		return 0, 0, false
	}
	j := strings.IndexByte(raw[i:], ')')
	if j < 0 {
		return 0, 0, false
	}
	inner := raw[i+1 : i+j]
	parts := strings.SplitN(inner, ",", 2)
	n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || n <= 0 {
		return 0, 0, false
	}
	first = n
	if len(parts) == 2 {
		if m, err2 := strconv.Atoi(strings.TrimSpace(parts[1])); err2 == nil && m >= 0 {
			second = m
		}
	}
	return first, second, true
}

// ServerIdentity 是连接后采集的真实服务器指纹，用于防误连。
type ServerIdentity struct {
	Hostname   string `json:"hostname,omitempty"`
	Instance   string `json:"instance,omitempty"`
	Version    string `json:"version,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
	// Fingerprint 是稳定指纹（主机 + 实例标识 + 版本大版本），用于检测目标变更。
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Capabilities 声明引擎能力。UI 据此渲染或置灰，service 据此拒绝非法调用。
//
// 禁止用 switch engine 代替本结构。
type Capabilities struct {
	Transactions      bool `json:"transactions"`
	Schemas           bool `json:"schemas"`
	Views             bool `json:"views"`
	MaterializedViews bool `json:"materialized_views"`
	Routines          bool `json:"routines"`
	Triggers          bool `json:"triggers"`
	Sequences         bool `json:"sequences"`
	UserTypes         bool `json:"user_types"`
	Indexes           bool `json:"indexes"`
	// AlterColumnNullability / AlterColumnDefault 表示引擎支持原地改可空性与
	// 默认值。SQLite 两者都不支持（只能重建表），据此拒绝而不是生成必然失败的语句。
	AlterColumnNullability bool `json:"alter_column_nullability"`
	AlterColumnDefault     bool `json:"alter_column_default"`
	// AddCheck 表示引擎的 CHECK 约束是**真的会被强制**的。
	//
	// MySQL 5.7 会解析 CHECK 但静默忽略（8.0.16 起才真正强制），SQLite 只能在
	// 建表时写 CHECK、加不上。这两种情况都必须声明 false，否则我们就是在
	// 「看起来建好了、约束不存在」的假成功上报平安。
	AddCheck bool `json:"add_check"`
	// AddTrigger 表示能用一条语句建触发器。
	//
	// PostgreSQL 要先 CREATE FUNCTION，再 CREATE TRIGGER——两步且函数体是
	// dollar-quoted 文本，v1 没做，所以 PG 为 false；MySQL / SQLite 都是单条语句。
	AddTrigger bool `json:"add_trigger"`
	// IndexValidity 表示 DescribeTable 会给出 Index.Valid（目前只有 PostgreSQL）。
	// 套件据此要求「声明了就真给」，避免字段写完没人查、上层拿到一片 nil。
	IndexValidity  bool `json:"index_validity"`
	Explain        bool `json:"explain"`
	SlowQueryStats bool `json:"slow_query_stats"`
	SessionMgmt    bool `json:"session_mgmt"`
	KillSession    bool `json:"kill_session"`
	// Metrics 表示驱动实现了 driver.MetricsProvider（连接数、缓存命中）。
	// 与 SessionMgmt 分开：会话列表与性能计数器是两套系统表，能列会话不代表
	// 拿得到计数器，反过来也一样。
	Metrics          bool `json:"metrics"`
	LockGraph        bool `json:"lock_graph"`
	Backup           bool `json:"backup"`
	BulkLoad         bool `json:"bulk_load"`
	KeysetPagination bool `json:"keyset_pagination"`
	// DDLTransactional 为 true 时 DDL 可真实回滚（PG / SQLite / 多数 MSSQL DDL）。
	DDLTransactional bool `json:"ddl_transactional"`
	// AlterColumnType 表示引擎支持原地修改列类型。
	// SQLite 为 false：它只能重建表，因此上层必须给出明确拒绝而不是生成必然失败的语句。
	// 见 ADR-0005：能力有无写进 Capabilities，语句文本差异归 dialect。
	AlterColumnType bool `json:"alter_column_type"`
	// AddForeignKey 表示引擎支持用 ALTER TABLE 追加外键约束。
	// SQLite 为 false：它的 ALTER TABLE 只认 RENAME / ADD COLUMN，加约束必须重建整表。
	AddForeignKey bool `json:"add_foreign_key"`
	// AddRoutine 表示能用表单建例程（存储过程 / 函数）。
	//
	// 与 Routines（能列出）分开：MySQL 能列出过程与函数，但**建函数**在开启
	// binlog 的实例上要求声明 DETERMINISTIC / READS SQL DATA / NO SQL，否则直接
	// 报 1418。给用户一个「大概率建不出来」的按钮，不如明确拒绝并指路手写 DDL。
	AddRoutine bool `json:"add_routine"`
}

// Connection 是一个可复用的连接配置。它不持有物理连接。
type Connection struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Engine  Engine `json:"engine"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
	Catalog string `json:"catalog,omitempty"`
	Schema  string `json:"schema,omitempty"`

	// Environment 默认为 unknown，策略严格度等同 production。
	Environment       Environment `json:"environment"`
	EnvironmentSource string      `json:"environment_source,omitempty"` // declared | detected | inherited

	AccessMode AccessMode `json:"access_mode"`

	// DisplayTimeZone 是时间值的展示时区（IANA 名）；空表示 UTC。
	// 只影响展示：会话时区固定 UTC，写进去的就是 UTC。
	DisplayTimeZone string `json:"display_timezone,omitempty"`

	// ServerIdentity 是运行时采集，不持久化敏感信息。
	ServerIdentity *ServerIdentity `json:"server_identity,omitempty"`
}

// DefaultAccessMode 返回某环境下的新建连接默认访问模式。
// 生产/预发默认只读，测试/本地默认读写。
func DefaultAccessMode(env Environment) AccessMode {
	if env.Strictness() >= 2 {
		return AccessReadOnly
	}
	return AccessReadWrite
}
