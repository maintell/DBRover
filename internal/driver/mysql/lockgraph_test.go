package mysql

import (
	"strings"
	"testing"
)

// TestPickLockWaitSQLByMajor 锁住 MySQL 5.7 / 8.0 两代锁等待表的分派。
//
// 实测背景（真实 MySQL 8.0.39 @192.168.2.2:3306/test_sit，只读）：
// MySQL 8.0 把 InnoDB 锁等待从 information_schema.innodb_lock_waits 迁到了
// performance_schema.data_lock_waits，前者在 8.0 直接报
// 「Unknown table 'INNODB_LOCK_WAITS' in information_schema」。此前驱动只会
// 查 information_schema，导致「锁等待 / 阻塞链」在 8.0 上整块坏掉，而既有
// 真机验收全在 5.7 上跑，从没暴露。
//
// 分派做成纯函数是为了能在无数据库的机器上被单测覆盖；SQL 是否在真库可跑
// 由 mysql-tools 只读实测 + 可选的 DBRover_TEST_MYSQL 集成用例保证。
func TestPickLockWaitSQLByMajor(t *testing.T) {
	cases := []struct {
		major int
		want  string
	}{
		{5, lockWaitSQL57},
		{7, lockWaitSQL57}, // MariaDB 等仍用 information_schema
		{8, lockWaitSQL80},
		{9, lockWaitSQL80}, // 未来主版本沿用 performance_schema
	}
	for _, c := range cases {
		if got := pickLockWaitSQL(c.major); got != c.want {
			t.Errorf("major=%d: 期望 %q 对应的 SQL，实际拿到另一条", c.major, c.want)
		}
	}
}

// 反向断言：两条 SQL 必须真的指向各自的表，不能有个版本退化成另一条。
func TestLockWaitSQLTargetsExpectedTables(t *testing.T) {
	if !strings.Contains(lockWaitSQL57, "information_schema.innodb_lock_waits") {
		t.Error("5.7 的 SQL 没有指向 information_schema.innodb_lock_waits")
	}
	if strings.Contains(lockWaitSQL57, "performance_schema.data_lock_waits") {
		t.Error("5.7 的 SQL 不该引用 8.0 的 performance_schema.data_lock_waits")
	}
	if !strings.Contains(lockWaitSQL80, "performance_schema.data_lock_waits") {
		t.Error("8.0 的 SQL 没有指向 performance_schema.data_lock_waits")
	}
	if strings.Contains(lockWaitSQL80, "information_schema.innodb_lock_waits") {
		t.Error("8.0 的 SQL 不该引用已移除的 information_schema.innodb_lock_waits")
	}
	// 8.0 路径必须经 threads 把 THREAD_ID 映射回 PROCESSLIST_ID，否则界面拿不到会话 id。
	if !strings.Contains(lockWaitSQL80, "PROCESSLIST_ID") || !strings.Contains(lockWaitSQL80, "performance_schema.threads") {
		t.Error("8.0 的 SQL 没有经 performance_schema.threads 映射 PROCESSLIST_ID")
	}
}

// 版本串解析：SELECT VERSION() 可能带后缀（8.0.39-log / 5.7.38 / 10.11.2-MariaDB）。
func TestParseMajorVersion(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"8.0.39", 8},
		{"8.0.39-log", 8},
		{"5.7.38", 5},
		{"10.11.2-MariaDB", 10},
		{"8", 8},     // 无点无后缀
		{"8-log", 8}, // 主版本带后缀
		{"8.0", 8},
		{"", 0},
		{"garbage", 0},
	}
	for _, c := range cases {
		if got := parseMajorVersion(c.in); got != c.want {
			t.Errorf("parseMajorVersion(%q)=%d, 期望 %d", c.in, got, c.want)
		}
	}
}
