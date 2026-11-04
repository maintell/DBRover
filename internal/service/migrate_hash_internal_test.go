package service

import (
	"testing"
	"time"
)

// TestCanonicalValue 钉住「同一份数据在不同驱动下编码相同」这条。
func TestCanonicalValue(t *testing.T) {
	// 整数与同值字符串必须编码一致：不同驱动对 DECIMAL/INT 的返回类型不统一，
	// 编成两种文本就会把「数据一样」判成不一致。
	if canonicalValue(int64(1)) != canonicalValue("1") {
		t.Errorf("int64(1) 与 \"1\" 的编码应当相同：%q vs %q",
			canonicalValue(int64(1)), canonicalValue("1"))
	}
	// NULL 与空串必须不同——这正是迁移里最常见的丢数据方式。
	if canonicalValue(nil) == canonicalValue("") {
		t.Error("NULL 与空串不能编码相同")
	}
	// 时间统一到 UTC：驱动给的时区不同不该影响指纹。
	sh := time.Date(2026, 11, 5, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))
	utc := sh.UTC()
	if canonicalValue(sh) != canonicalValue(utc) {
		t.Error("同一瞬间的不同时区表示应编码相同")
	}
	if canonicalValue([]byte{0x01, 0xff}) != "x'01ff'" {
		t.Errorf("二进制编码 = %q", canonicalValue([]byte{0x01, 0xff}))
	}
}

// TestTableDigest 钉住累加语义：行数与十六进制摘要。
func TestTableDigest(t *testing.T) {
	var a, b tableDigest
	rows := [][]any{{int64(1), "x"}, {int64(2), nil}}
	for _, r := range rows {
		a.Add(r)
	}
	for _, r := range rows {
		b.Add(r)
	}
	if a.Rows() != 2 {
		t.Errorf("行数 = %d", a.Rows())
	}
	if a.Hex() != b.Hex() {
		t.Error("同样的行按同样顺序累加，摘要必须相同")
	}

	// 换顺序必须得到不同摘要：这正是「按主键排序」这条要求的原因。
	var c tableDigest
	c.Add(rows[1])
	c.Add(rows[0])
	if c.Hex() == a.Hex() {
		t.Error("顺序不同应得到不同摘要（调用方必须按主键排序）")
	}
	t.Logf("digest=%s", a.Hex())
}
