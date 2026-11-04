package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// nullSentinel 是 NULL 的固定编码。
//
// 用 \x00 前缀是因为它在正文里不可能出现（文本列里出现 NUL 会被驱动拒绝），
// 而空串编码后是空串——两者必须能区分，否则「NULL 变成 ”」这种经典丢数据
// 会被哈希判成一致。
const nullSentinel = "\x00NULL"

// canonicalValue 把一个列值编码成可比较的文本（docs/08 ADR-0011 §3）。
//
// 目标：**同一份数据在不同驱动下编码相同**。驱动返回的类型并不统一（整数可能
// 是 int64 也可能是字符串、时间可能是 time.Time 也可能是 RFC3339 文本），所以
// 不能直接 fmt.Sprint 了事。
func canonicalValue(v any) string {
	switch x := v.(type) {
	case nil:
		return nullSentinel
	case []byte:
		return "x'" + hex.EncodeToString(x) + "'"
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

// rowDigest 返回一行的指纹：列值用 \x1f 分隔（避免拼接歧义），再取 SHA-256。
//
// 单行哈希不是为了「按行比对」（那要求两端顺序一致），而是为了让整表哈希可以
// 流式累加：把每行指纹按顺序喂给 tableDigest 即可。
func rowDigest(vals []any) [32]byte {
	var b strings.Builder
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(0x1f)
		}
		b.WriteString(canonicalValue(v))
	}
	return sha256.Sum256([]byte(b.String()))
}

// tableDigest 把一串行指纹累加成整表哈希。
//
// 顺序敏感：调用方必须按主键排序后再喂（ADR-0011 §3），否则两端顺序不同会
// 得出不同结果——那是「顺序不同」，不是「数据不同」。
type tableDigest struct {
	h    [32]byte
	n    int64
	init bool
}

// Add 累加一行。
func (d *tableDigest) Add(vals []any) {
	row := rowDigest(vals)
	if !d.init {
		d.h = row
		d.init = true
	} else {
		var buf [64]byte
		copy(buf[:32], d.h[:])
		copy(buf[32:], row[:])
		d.h = sha256.Sum256(buf[:])
	}
	d.n++
}

// Hex 返回十六进制摘要。
func (d *tableDigest) Hex() string { return hex.EncodeToString(d.h[:]) }

// Rows 返回已累加的行数。
func (d *tableDigest) Rows() int64 { return d.n }
