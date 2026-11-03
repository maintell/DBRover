package service

import (
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestNormalizeTemporal 锁住时间值写回的两条规则：
// 带时区的列存瞬间（换算到 UTC），不带时区的列存墙上时钟（原样写）。
//
// 这条规则错一次的后果很具体：用户看到 12:00，写回后变成 04:00，
// 而库里没有任何报错——数据静默挪了 8 小时。
func TestNormalizeTemporal(t *testing.T) {
	tsTZ := model.ColumnType{Kind: model.TypeTimestamp, WithTZ: true}
	tsNoTZ := model.ColumnType{Kind: model.TypeTimestamp}
	date := model.ColumnType{Kind: model.TypeDate}
	tm := model.ColumnType{Kind: model.TypeTime}

	cases := []struct {
		name string
		in   string
		ct   model.ColumnType
		want string
	}{
		{"带时区列换算到 UTC", "2026-11-03T12:00:00+08:00", tsTZ, "2026-11-03 04:00:00"},
		{"带时区列本来就是 UTC", "2026-11-03T04:00:00Z", tsTZ, "2026-11-03 04:00:00"},
		{"不带时区列保持墙上时钟", "2026-11-03T12:00:00+08:00", tsNoTZ, "2026-11-03 12:00:00"},
		{"DATE 取字面日期，不跨零点", "2026-11-03T02:00:00+08:00", date, "2026-11-03"},
		{"TIME 取字面时间", "2026-11-03T12:34:56+08:00", tm, "12:34:56"},
		// 已经是引擎原生写法：不猜，原样交给引擎。
		{"原生写法原样透传", "2026-11-03 12:00:00", tsTZ, "2026-11-03 12:00:00"},
		{"空串原样", "", tsTZ, ""},
		{"带纳秒", "2026-11-03T12:00:00.123456789+08:00", tsTZ, "2026-11-03 04:00:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeTemporal(c.in, c.ct); got != c.want {
				t.Errorf("normalizeTemporal(%q) = %q, 期望 %q", c.in, got, c.want)
			}
		})
	}
}

// TestNormalizeTemporalMapOnlyTouchesTemporalColumns 验证只动时间列，
// 别的列（尤其文本）一点都不能碰。
func TestNormalizeTemporalMapOnlyTouchesTemporalColumns(t *testing.T) {
	cols := map[string]model.Column{
		"id":  {Name: "id", Type: model.ColumnType{Kind: model.TypeInteger}},
		"at":  {Name: "at", Type: model.ColumnType{Kind: model.TypeTimestamp, WithTZ: true}},
		"txt": {Name: "txt", Type: model.ColumnType{Kind: model.TypeText}},
	}
	in := map[string]any{
		"id":  1,
		"at":  "2026-11-03T12:00:00+08:00",
		"txt": "2026-11-03T12:00:00+08:00",
		"who": "2026-11-03T12:00:00+08:00", // 表里没有这列
	}
	out := normalizeTemporalMap(cols, in)

	if out["at"] != "2026-11-03 04:00:00" {
		t.Errorf("时间列应换算, 实际 %v", out["at"])
	}
	if out["txt"] != "2026-11-03T12:00:00+08:00" {
		t.Errorf("文本列不该被改, 实际 %v", out["txt"])
	}
	if out["who"] != "2026-11-03T12:00:00+08:00" {
		t.Errorf("未知列不该被改, 实际 %v", out["who"])
	}
	if out["id"] != 1 {
		t.Errorf("整数列不该被改, 实际 %v", out["id"])
	}
	// 返回的是新 map，原 map 不被修改——请求体可能还要被别处复用。
	if in["at"] != "2026-11-03T12:00:00+08:00" {
		t.Errorf("不该原地修改入参, 实际 %v", in["at"])
	}
}
