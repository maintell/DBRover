package driver

import "testing"

// TestStringifyLargeInts 钉住「只动会有损的值」这条边界。
//
// 两个方向都要断言：漏掉一个（2^53+1 保持数字）会让界面静默丢精度；
// 多转一个（2^53 以内也变字符串）会让排序、求和、列宽估算全按文本处理。
func TestStringifyLargeInts(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"int64 边界内", int64(9007199254740992), int64(9007199254740992)},
		{"int64 边界外", int64(9007199254740993), "9007199254740993"},
		{"int64 最大值", int64(9223372036854775807), "9223372036854775807"},
		{"int64 负边界外", int64(-9007199254740993), "-9007199254740993"},
		{"uint64 边界外", uint64(18446744073709551615), "18446744073709551615"},
		{"uint64 边界内", uint64(9007199254740992), uint64(9007199254740992)},
		{"字符串不动", "9007199254740993", "9007199254740993"},
		{"浮点不动", 1.5, 1.5},
		{"nil 不动", nil, nil},
	}
	for _, c := range cases {
		vals := []any{c.in}
		StringifyLargeInts(vals)
		if vals[0] != c.want {
			t.Errorf("%s：StringifyLargeInts(%v) = %#v，期望 %#v", c.name, c.in, vals[0], c.want)
		}
	}
}

// TestStringifyLargeIntsWalksWholeRow 防止只处理第一列。
func TestStringifyLargeIntsWalksWholeRow(t *testing.T) {
	vals := []any{int64(1), int64(9007199254740993), "x"}
	StringifyLargeInts(vals)
	if vals[0] != int64(1) {
		t.Errorf("第一列被误改：%#v", vals[0])
	}
	if vals[1] != "9007199254740993" {
		t.Errorf("第二列没被处理：%#v", vals[1])
	}
	if vals[2] != "x" {
		t.Errorf("第三列被误改：%#v", vals[2])
	}
}
