package service

import "time"

// MetricSizePrefix 是「表体量」在留存里用的指标前缀。
//
// 用前缀 + 表名放进同一张 metric_sample，而不是新开一张表：ADR-0016 的表结构
// 够用，改结构要升 user_version，为一个派生指标不值得。
const MetricSizePrefix = "size:"

// GrowthOf 用留存里的历史点算一张表的体积增长。
//
// 只有**两个以上**的点才谈得上增长：一个点只能说明「现在是多大」。这也正是
// 「先有留存才有增长」的意思——没有历史就只能说实话（ok=false），不能编一个 0。
func GrowthOf(points []MetricPoint, current int64) (delta int64, hours float64, ok bool) {
	if len(points) < 2 {
		return 0, 0, false
	}
	first := points[0]
	if first.TS <= 0 {
		return 0, 0, false
	}
	span := time.Since(time.Unix(first.TS, 0)).Hours()
	if span <= 0 {
		return 0, 0, false
	}
	return current - int64(first.Value), span, true
}
