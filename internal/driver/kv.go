package driver

import (
	"context"
	"time"

	"github.com/maintell/DBRover/internal/model"
)

// KVType 是 key-value 引擎里的值类型。
//
// 只列关系型引擎没有对应物的那几类；没有「表/列」的概念，所以也没有 TableDetail。
type KVType string

const (
	KVString KVType = "string"
	KVHash   KVType = "hash"
	KVList   KVType = "list"
	KVSet    KVType = "set"
	KVZSet   KVType = "zset"
	KVStream KVType = "stream"
	// KVNone 表示 key 不存在。
	KVNone KVType = "none"
	// KVOther 覆盖位图 / HyperLogLog / geo / 模块类型：能看，但按不透明值处理。
	KVOther KVType = "other"
)

// KVScanOptions 是一次 keyspace 遍历的参数。
type KVScanOptions struct {
	// Cursor 是上一页返回的游标；空表示从头开始。
	// 必须是引擎自己的游标而不是 offset：SCAN 的语义不允许按序号跳。
	Cursor  string
	Pattern string
	// Type 非空时只返回该类型的 key。
	Type  KVType
	Count int
}

// KVPage 是一页 key。HasMore 为 true 时 Cursor 才有意义。
type KVPage struct {
	Keys    []string `json:"keys"`
	Cursor  string   `json:"cursor"`
	HasMore bool     `json:"has_more"`
}

// KVEntry 是一个 key 的元信息。SizeBytes 为 nil 表示没测出来（不猜成 0）。
type KVEntry struct {
	Key       string `json:"key"`
	Type      KVType `json:"type"`
	TTL       int64  `json:"ttl_seconds"` // -1 表示永不过期；-2 表示 key 不存在
	Encoding  string `json:"encoding,omitempty"`
	Length    *int64 `json:"length,omitempty"`     // 元素数：STRLEN/HLEN/LLEN/SCARD/ZCARD/XLEN
	SizeBytes *int64 `json:"size_bytes,omitempty"` // MEMORY USAGE；可能阻塞，未测时为 nil
	Preview   string `json:"preview,omitempty"`    // 限长预览，不是全量
}

// KVValueOptions 是按类型读值的参数。
type KVValueOptions struct {
	Cursor string
	Count  int
	// MaxBytes 只对 string 生效：超出部分不取，并置 Truncated。
	MaxBytes int
}

// KVPair 是 hash 的一个 field、zset 的一个成员等。
type KVPair struct {
	Field  string  `json:"field,omitempty"`
	Value  string  `json:"value,omitempty"`
	Score  float64 `json:"score,omitempty"`
	Member string  `json:"member,omitempty"`
	ID     string  `json:"id,omitempty"` // stream 的 entry id
}

// KVValue 是一个 key 的值（分页）。Truncated 必须显式给：读够上限与读尽不同。
type KVValue struct {
	Key       string   `json:"key"`
	Type      KVType   `json:"type"`
	String    *string  `json:"string,omitempty"`
	Pairs     []KVPair `json:"pairs,omitempty"`
	Cursor    string   `json:"cursor,omitempty"`
	HasMore   bool     `json:"has_more"`
	Truncated bool     `json:"truncated,omitempty"`
}

// KVMutationKind 是写操作的类型。
//
// 前缀 KVMut 是刻意的：值类型里已经有 KVSet（set 类型），
// 两个同名常量放在一个包里只会让人写错。
type KVMutationKind string

const (
	KVMutSetString  KVMutationKind = "set_string"  // string 覆盖写
	KVMutSetField   KVMutationKind = "set_field"   // hash field
	KVMutDeleteKey  KVMutationKind = "delete_key"  // DEL / UNLINK
	KVMutDeleteItem KVMutationKind = "delete_item" // hash field / list 元素 / set 成员 / zset 成员
	KVMutPush       KVMutationKind = "push"        // list push
	KVMutExpire     KVMutationKind = "expire"
)

// KVMutation 是一次写操作。它会被执行门分级、按需审批、记审计。
type KVMutation struct {
	Kind   KVMutationKind `json:"kind"`
	Key    string         `json:"key"`
	Field  string         `json:"field,omitempty"`
	Value  string         `json:"value,omitempty"`
	Member string         `json:"member,omitempty"`
	Score  *float64       `json:"score,omitempty"`
	TTL    int64          `json:"ttl_seconds,omitempty"`
}

// KVResult 是写操作或命令的执行结果。
type KVResult struct {
	// Rows 是返回的行（命令模式可能有多行）。
	Rows [][]any `json:"rows,omitempty"`
	// Changed 是受影响元素数；未知时为 -1，不假装是 0。
	Changed  int64    `json:"changed"`
	Warnings []string `json:"warnings,omitempty"`
}

// KVConnector 由 key-value 引擎实现。
//
// 它**不实现** Connector：没有表、没有列、没有 SQL，硬套只会得到一堆永远返空
// 的方法，让上层误判能力存在（ADR-0007）。加新引擎时 api 靠类型断言选路径，
// 不判断引擎名。
type KVConnector interface {
	Engine() model.Engine
	Capabilities() model.Capabilities
	Open(ctx context.Context, cfg ConnectConfig) error
	Close() error
	Health(ctx context.Context) (*HealthInfo, error)

	// Scan 按游标遍历 key。禁止实现成 KEYS：集群模式下没有它，
	// 单机模式下它会阻塞整个实例。
	Scan(ctx context.Context, opt KVScanOptions) (*KVPage, error)
	// Inspect 读一个 key 的类型、TTL、编码、元素数与体积。
	Inspect(ctx context.Context, key string) (*KVEntry, error)
	// Value 按类型分页读值。
	Value(ctx context.Context, key string, opt KVValueOptions) (*KVValue, error)
	// Mutate 执行一次结构化写操作；风险分级由 service 负责，驱动只管执行。
	Mutate(ctx context.Context, op KVMutation) (*KVResult, error)
	// Command 是命令模式的逃生口：结构化命令名 + 参数，不是自由文本。
	Command(ctx context.Context, name string, args []any) (*KVResult, error)
}

// KVDefaultScanCount 是 SCAN 每页的默认条数。
// 太小会让大 keyspace 翻页次数爆炸，太大则单次阻塞时间变长。
const KVDefaultScanCount = 200

// KVDefaultPreviewBytes 是 string 预览的默认上限（8 KB）。
// 一个 500 MB 的 key 不能整取，预览必须限长且显式标 truncated。
const KVDefaultPreviewBytes = 8 * 1024

// KVInspectTimeout 是 MEMORY USAGE 这类可能阻塞的探测的超时。
// 超时就返回「测不出来」（SizeBytes = nil），而不是让整个请求挂在那里。
const KVInspectTimeout = 2 * time.Second
