package driver

import (
	"strconv"
)

// 本文件是读路径的最后一公里：把驱动扫出来的原生值调整成能安全穿过 JSON 的
// 形态。所有读路径（表浏览、查询分页、导出、游标续页、Agent 工具）都经过三个
// 驱动的 cursor，这里改一次就全都改到。

// JSMaxSafeInt 是 JSON 数字在 JavaScript 侧能精确表示的整数上限（2^53）。
//
// 为什么是 JS 而不是 JSON 规范：JSON 本身不限制位数，但界面（浏览器）、
// 以及绝大多数 SDK 都会把整数解析成双精度浮点。超过 2^53 时低位直接丢失——
// 9223372036854775807 读出来会变成 …5808，而界面上的数字看起来完全正常。
const JSMaxSafeInt int64 = 1 << 53

// StringifyLargeInts 把超出安全范围的整数改写成十进制字符串（原地修改）。
//
// 契约见 docs/05 §3.1「Decimal / BigInt 以字符串传输」。这里只动**会有损**的值：
// 2^53 以内的整数保持数字形态，界面的排序、求和、列宽估算都还按数字走。
// 全量字符串化等于把「这个值有没有超范围」的判断推给每个调用方，而它们
// 大多只会 parseInt 回来。
//
// 与写路径是对称的：service.coerceKey 会把字符串主键还原成整数做键集分页，
// 变更集也接受字符串形式的大整数（service.ErrLargeNumberAsJSON 要求必须用
// 字符串传，否则在解析阶段就已经丢精度）。
func StringifyLargeInts(vals []any) {
	for i, v := range vals {
		if s, ok := largeIntText(v); ok {
			vals[i] = s
		}
	}
}

// largeIntText 判断一个值是否是需要字符串化的大整数。
func largeIntText(v any) (string, bool) {
	switch n := v.(type) {
	case int64:
		if n > JSMaxSafeInt || n < -JSMaxSafeInt {
			return strconv.FormatInt(n, 10), true
		}
	case uint64:
		if n > uint64(JSMaxSafeInt) {
			return strconv.FormatUint(n, 10), true
		}
	case int:
		if int64(n) > JSMaxSafeInt || int64(n) < -JSMaxSafeInt {
			return strconv.Itoa(n), true
		}
	}
	return "", false
}
