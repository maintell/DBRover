package driver

import "context"

// LockWait 是一条「谁在等谁」：一个会话被另一个会话挡住。
//
// 只给**边**（等待者 → 持有者），不给整张图：调用方要的是树，而树可以从边算出来；
// 反过来让每个驱动各自拼树，就会出现三种形状。
type LockWait struct {
	// WaitingID / BlockingID 是会话 id（与 SessionInfo.ID 同一套编号）。
	WaitingID  string `json:"waiting_id"`
	BlockingID string `json:"blocking_id"`
	// WaitSeconds 是已经等了多久；-1 表示引擎没给。
	WaitSeconds int64 `json:"wait_seconds"`
	// WaitingSQL / BlockingSQL 是两个会话当前/最后执行的语句（可能为空）。
	WaitingSQL  string `json:"waiting_sql,omitempty"`
	BlockingSQL string `json:"blocking_sql,omitempty"`
}

// LockGraphProvider 是「锁与阻塞链」的可选能力（FR-MON-03）。
//
// 判据是 Capabilities().LockGraph。目前只做**行锁/表锁的等待关系**；metadata lock
// 那一层要 MySQL 8 的 performance_schema.metadata_locks，5.7 拿不到（docs/07
// 验收 11 同一个卡点），如实不假装能看。
type LockGraphProvider interface {
	// LockWaits 列出当前的锁等待边。没有等待时返回空切片而不是 nil。
	LockWaits(ctx context.Context) ([]LockWait, error)
}
