package drivertest

import (
	"context"
	"testing"
)

// testLargeInts 锁住「超过 2^53 的整数以字符串回传」（docs/05 §3.1）。
//
// 为什么必须在真库上测：这条规则不是在 JSON 编码那一层拦的，而是在每个驱动
// 的 cursor 里把 int64 换成字符串。少接一个引擎，那个引擎上读出来的大整数
// 就是「看起来正常、其实已经少了 1」——单测拿构造出来的值测不出这一点，
// 必须走一次真实的扫描、类型推导与归一化。
//
// 反向断言（42 仍然必须是数字）同样是必须的：如果实现把整型列一律转成字符串，
// 前一条断言照样通过，界面的排序与列宽估算却全废了。
func testLargeInts(t *testing.T, cfg Config) {
	const (
		big   = "9007199254740993" // 2^53 + 1：双精度会丢成 …992
		small = int64(42)
	)
	c := open(t, cfg, false)
	s := session(t, c)
	ctx := context.Background()

	_, _ = s.Execute(ctx, "DROP TABLE IF EXISTS dbv_large_int", nil)
	mustExec(t, s, "CREATE TABLE dbv_large_int (id BIGINT NOT NULL PRIMARY KEY)")
	t.Cleanup(func() {
		_, _ = s.Execute(context.Background(), "DROP TABLE IF EXISTS dbv_large_int", nil)
	})
	mustExec(t, s, "INSERT INTO dbv_large_int (id) VALUES ("+big+")")
	mustExec(t, s, "INSERT INTO dbv_large_int (id) VALUES ("+itoa64(small)+")")

	rows := queryAll(t, s, "SELECT id FROM dbv_large_int ORDER BY id")
	if len(rows) != 2 {
		t.Fatalf("期望 2 行，实际 %d 行: %v", len(rows), rows)
	}

	// 排序后小的在前。
	switch v := rows[0][0].(type) {
	case string:
		t.Fatalf("2^53 以内的整数不该被字符串化（会破坏排序与聚合），实际是 %q", v)
	case int64:
		if v != small {
			t.Fatalf("小整数读回 %v，期望 %d", v, small)
		}
	default:
		t.Fatalf("2^53 以内的整数读回类型 %T（%v），期望 int64", rows[0][0], rows[0][0])
	}

	got, isStr := rows[1][0].(string)
	if !isStr {
		t.Fatalf("%s 读回的类型是 %T（%v）：超过 2^53 的整数必须字符串化，否则界面拿到的是丢了低位的数",
			big, rows[1][0], rows[1][0])
	}
	if got != big {
		t.Fatalf("大整数读回 %q，期望 %q（低位不能变）", got, big)
	}
}

// itoa64 避免为新用例引入 strconv 之外的依赖。
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
