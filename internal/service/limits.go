package service

// 结果集的两条硬边界（docs/05 §8）。
//
// 为什么要有上限而不是「让它跑完」：一次 SELECT * 命中千万行的场景在真实
// 运维里天天出现，没有上限的实现会先把服务端内存吃光，再让用户以为程序卡死。
// 上限不是能力缺陷，是让「停下来」这件事变得可预期、可解释。
const (
	// DefaultMaxRows 是一次执行默认返回的行数。够看一段样本，又不至于把响应撑爆。
	DefaultMaxRows = 1000
	// MaxRowsCeiling 是单条查询最多能取多少行。给了再大的数也按它算：
	// 超过十万行的结果应该走流式游标分页或导出，而不是塞进一个 JSON 响应。
	MaxRowsCeiling = 100000
	// DefaultPageSize 是结果区分页的默认页大小。
	DefaultPageSize = 200
	// MaxPageSize 是单页上限。界面可选 50–2000，超过按 2000 处理。
	MaxPageSize = 2000
)

// clampMaxRows 把调用方给的行数上限收进合法区间。
func clampMaxRows(n int) int {
	if n <= 0 {
		return DefaultMaxRows
	}
	if n > MaxRowsCeiling {
		return MaxRowsCeiling
	}
	return n
}

// clampPageSize 把调用方给的页大小收进合法区间。
//
// 下限刻意留 1：内部调用方（Agent 的 data.sample、测试）需要拿两三行看形状，
// 按「界面最少 50 行」去卡它们只会逼出一堆例外。50 这个数字是界面的可选项
// 边界，不是协议约束。
func clampPageSize(n int) int {
	if n <= 0 {
		return DefaultPageSize
	}
	if n > MaxPageSize {
		return MaxPageSize
	}
	return n
}
