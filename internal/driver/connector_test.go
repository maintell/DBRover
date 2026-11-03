package driver_test

import (
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

// TestDisplayLocation 锁住「展示时区解析」的两条规则：
// 空 = UTC；非法名也退回 UTC，绝不按服务器本地时区猜。
func TestDisplayLocation(t *testing.T) {
	if loc := (driver.ConnectConfig{}).DisplayLocation(); loc != time.UTC {
		t.Errorf("空值应退回 UTC, 实际 %v", loc)
	}
	if loc := (driver.ConnectConfig{DisplayTimeZone: "UTC"}).DisplayLocation(); loc != time.UTC {
		t.Errorf("UTC 应原样返回, 实际 %v", loc)
	}

	loc := driver.ConnectConfig{DisplayTimeZone: "Asia/Shanghai"}.DisplayLocation()
	if loc.String() != "Asia/Shanghai" {
		t.Fatalf("合法名应解析成功, 实际 %v", loc)
	}
	// 上海是 +8，且不带夏令时。
	if _, off := time.Date(2026, 11, 3, 0, 0, 0, 0, loc).Zone(); off != 8*3600 {
		t.Errorf("Asia/Shanghai 偏移 = %d 秒, 期望 28800", off)
	}

	if loc := (driver.ConnectConfig{DisplayTimeZone: "Asia/Shanghia"}).DisplayLocation(); loc != time.UTC {
		t.Errorf("拼错的时区名应退回 UTC, 实际 %v", loc)
	}
}

// TestConvertTimeValues 验证只动 time.Time，其余值原样保留。
func TestConvertTimeValues(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载时区: %v", err)
	}
	utc := time.Date(2026, 11, 3, 4, 0, 0, 0, time.UTC)

	vals := []any{int64(7), "text", utc, nil}
	driver.ConvertTimeValues(vals, shanghai)

	if vals[0] != int64(7) || vals[1] != "text" || vals[3] != nil {
		t.Errorf("非时间值被改动了: %+v", vals)
	}
	got, ok := vals[2].(time.Time)
	if !ok {
		t.Fatalf("时间值类型变了: %T", vals[2])
	}
	// 同一个瞬间，只是换了时区显示。
	if !got.Equal(utc) {
		t.Errorf("换算改变了瞬间: %v vs %v", got, utc)
	}
	if h := got.Hour(); h != 12 {
		t.Errorf("上海时间的小时 = %d, 期望 12（UTC 04:00 + 8）", h)
	}

	// loc 为 nil 时不动任何东西，避免调用方漏判。
	vals2 := []any{utc}
	driver.ConvertTimeValues(vals2, nil)
	if vals2[0].(time.Time) != utc {
		t.Error("loc 为 nil 时不该改动")
	}
}
