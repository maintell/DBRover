package policy

import (
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestRedisCommandRisk 锁住命令风险表的关键判断。
//
// 这张表错了会很危险：把 FLUSHALL 判成 R2，生产环境上一次误点就清空整库；
// 把 GET 判成 R4，则每看一眼都要审批，工具没法用。
func TestRedisCommandRisk(t *testing.T) {
	cases := []struct {
		cmd    string
		want   Level
		reason Reason
	}{
		{"GET", R0, ReasonReadOnly},
		{"hgetall", R0, ReasonReadOnly}, // 大小写不敏感
		{"  TTL  ", R0, ReasonReadOnly}, // 首尾空白要吃掉
		{"SCAN", R1, ReasonTraversal},
		{"HSCAN", R1, ReasonTraversal},
		{"SET", R2, ReasonSingleKeyWrite},
		{"HSET", R2, ReasonSingleKeyWrite},
		{"DEL", R2, ReasonSingleKeyWrite},
		{"UNLINK", R2, ReasonSingleKeyWrite},
		{"KEYS", R3, ReasonExpensive},
		{"FLUSHALL", R4, ReasonDestructive},
		{"FLUSHDB", R4, ReasonDestructive},
		{"CONFIG", R4, ReasonAdminCommand},
		{"EVAL", R4, ReasonAdminCommand},
		{"CLIENT", R4, ReasonAdminCommand},
		// 未知命令按最危险处理：不能因为「没写在表里」就放行。
		{"SOMEBRANDNEWCOMMAND", R4, ReasonUnknownCommand},
		{"", R4, ReasonUnknownCommand},
	}
	for _, c := range cases {
		got, reason := RedisCommandRisk(c.cmd)
		if got != c.want {
			t.Errorf("RedisCommandRisk(%q) = %s, 期望 %s", c.cmd, got, c.want)
		}
		if reason != c.reason {
			t.Errorf("RedisCommandRisk(%q) 理由 = %q, 期望 %q", c.cmd, reason, c.reason)
		}
	}
}

// TestRedisCommandBlocked 只有两条命令直接拒绝：KEYS 与 DEBUG。
//
// 「不允许」与「要确认」是两件事：FLUSHALL 危险但有正当用途（测试库重置），
// 所以给审批而不是封死；KEYS 有完全等价的 SCAN，DEBUG 是调试后门，没有正当理由。
func TestRedisCommandBlocked(t *testing.T) {
	for _, cmd := range []string{"KEYS", "keys", " DEBUG "} {
		why, blocked := RedisCommandBlocked(cmd)
		if !blocked {
			t.Errorf("%q 应被直接拒绝", cmd)
		}
		if why == "" {
			t.Errorf("%q 被拒绝时必须说明替代方案", cmd)
		}
	}
	for _, cmd := range []string{"FLUSHALL", "EVAL", "GET", "SCAN"} {
		if why, blocked := RedisCommandBlocked(cmd); blocked {
			t.Errorf("%q 不该被封死（应走审批），理由: %s", cmd, why)
		}
	}

	// 被拒绝时给出的替代方案要能照着做。
	if why, _ := RedisCommandBlocked("KEYS"); !containsAll(why, "SCAN", "阻塞") {
		t.Errorf("KEYS 的拒绝理由应说明改用 SCAN 与原因: %s", why)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestRedisRiskCoversDocumentedFamilies 确认 ADR-0007 §3 里列出的每一族
// 都真的在表里——文档写了而代码没写，是最容易漏的一类不一致。
func TestRedisRiskCoversDocumentedFamilies(t *testing.T) {
	must := map[string]Level{
		"HGETALL": R0, "LRANGE": R0, "ZRANGE": R0, "XRANGE": R0,
		"SSCAN": R1, "MEMORY": R1,
		"LPUSH": R2, "SADD": R2, "ZADD": R2, "XADD": R2, "EXPIRE": R2,
		"RENAME": R2, "PERSIST": R2,
		"FLUSHDB": R4, "SWAPDB": R4,
		"SCRIPT": R4, "REPLICAOF": R4, "SHUTDOWN": R4,
	}
	for cmd, want := range must {
		if got, _ := RedisCommandRisk(cmd); got != want {
			t.Errorf("%s = %s, 文档写的是 %s", cmd, got, want)
		}
	}
}

var _ = model.EngineRedis
