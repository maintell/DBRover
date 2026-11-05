package driver

import (
	"context"
	"errors"
)

// ErrSlowQueryUnavailable 表示这个实例上拿不到慢查询统计。
//
// PG 需要装并 preload `pg_stat_statements`；MySQL 需要 performance_schema。
// 这不是错误路径而是**降级路径**：上层应把它翻成「怎么打开」的说明，而不是 5xx。
var ErrSlowQueryUnavailable = errors.New("这个实例没有开启慢查询统计")

// QueryStat 是一条按指纹聚合的语句统计。
//
// **这些是累计值**（自实例启动或上次重置以来），不是窗口值。差分由上层做：
// docs/09 §6.2 点名「把累计值当窗口值展示」是最容易做错的地方——用户会看到
// 「这条 SQL 跑了 120 万次」，而那是自启动以来的总量。
type QueryStat struct {
	// Digest 是引擎侧的指纹；同一指纹的多条语句已经聚合。
	Digest string `json:"digest"`
	// Text 是引擎给的归一化语句样本（字面量已被引擎替换）。
	Text string `json:"text"`
	// Calls 是累计执行次数。
	Calls int64 `json:"calls"`
	// TotalMS 是累计总耗时（毫秒）。
	TotalMS float64 `json:"total_ms"`
	// Rows 是累计返回/影响行数；-1 表示这个引擎没给。
	Rows int64 `json:"rows"`
}

// SlowQueryProvider 是「慢查询 Top N」的可选能力（FR-MON-04）。
//
// 判据是 Capabilities().SlowQueryStats。返回的是**累计值**，差分与窗口由服务层
// 负责——让每个驱动各写一套窗口逻辑，结果一定是三个引擎三种口径。
type SlowQueryProvider interface {
	// QueryStats 按总耗时倒序返回最多 limit 条累计统计。
	QueryStats(ctx context.Context, limit int) ([]QueryStat, error)
	// Hint 说明「怎么把这个引擎的慢查询统计打开」。不可用时原样展示给用户。
	Hint() string
}
