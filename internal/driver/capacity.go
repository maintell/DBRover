package driver

import "context"

// TableSize 是一张表的体量。
//
// 数据与索引分开给：只报总量的话，「为什么这张表这么大」还是要再点进去看一次。
// 不同引擎的口径不同（PG 的 pg_total_relation_size 含 TOAST 与索引；MySQL 是
// DATA_LENGTH + INDEX_LENGTH 的估算值），所以字段名用「引擎自己报的」这种中性说法，
// 界面不要在这些数字上做二次运算。
type TableSize struct {
	// Schema 是所在的 schema / 库；单库引擎里为空。
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
	// DataBytes / IndexBytes 是数据与索引的字节数；不区分的引擎把索引填 0。
	DataBytes  int64 `json:"data_bytes"`
	IndexBytes int64 `json:"index_bytes"`
	// TotalBytes 是引擎报的总量（PG 的 pg_total_relation_size / MySQL 的两者之和）。
	TotalBytes int64 `json:"total_bytes"`
	// Rows 是估算行数；-1 表示这个引擎没给（MySQL InnoDB 的 TABLE_ROWS 是估算值）。
	Rows int64 `json:"rows"`
}

// CapacityProvider 是「容量视图」的可选能力（FR-MON-06）。
//
// 只给体量，不给增长：增长需要本地时序留存（FR-MON-05），那是另一件事。把两者
// 混在一个接口里，会让「有体量」这件事必须先有时序库才能成立。
type CapacityProvider interface {
	// TableSizes 按总量倒序返回最多 limit 张表。
	TableSizes(ctx context.Context, limit int) ([]TableSize, error)
}
