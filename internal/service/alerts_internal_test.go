package service

import (
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

// TestAlertRules 钉住两条规则与两个抑制窗口（docs/09 §7）。
func TestAlertRules(t *testing.T) {
	s := &alertState{}
	t0 := time.Unix(1793840000, 0)

	// 连接占用：连续 3 次才触发，单次尖峰不该叫醒人。
	if got := s.evaluate(t0, 85, true, 0); len(got) != 0 {
		t.Fatalf("第 1 次不该触发，实际 %+v", got)
	}
	if got := s.evaluate(t0.Add(time.Minute), 85, true, 0); len(got) != 0 {
		t.Fatalf("第 2 次不该触发，实际 %+v", got)
	}
	got := s.evaluate(t0.Add(2*time.Minute), 85, true, 0)
	if len(got) != 1 || got[0].Rule != "连接占用" {
		t.Fatalf("第 3 次应触发一条连接占用告警，实际 %+v", got)
	}
	// 抑制窗口内不重复。
	if got := s.evaluate(t0.Add(10*time.Minute), 95, true, 0); len(got) != 0 {
		t.Errorf("30 分钟抑制窗口内不该重复，实际 %+v", got)
	}
	// 过了窗口再响一次。
	if got := s.evaluate(t0.Add(40*time.Minute), 95, true, 0); len(got) != 1 {
		t.Errorf("过了抑制窗口应再次触发，实际 %+v", got)
	}

	// 中断会清零：85 → 10 → 85 只算 1 次连续。
	s2 := &alertState{}
	s2.evaluate(t0, 85, true, 0)
	s2.evaluate(t0, 10, true, 0)
	s2.evaluate(t0, 85, true, 0)
	if got := s2.evaluate(t0, 85, true, 0); len(got) != 0 {
		t.Errorf("中间掉下来过就不该算连续 3 次，实际 %+v", got)
	}

	// 阻塞链：深度 > 3 才触发，抑制 5 分钟。
	s3 := &alertState{}
	if got := s3.evaluate(t0, 0, false, 3); len(got) != 0 {
		t.Errorf("深度 3 不该触发，实际 %+v", got)
	}
	if got := s3.evaluate(t0, 0, false, 4); len(got) != 1 {
		t.Errorf("深度 4 应触发，实际 %+v", got)
	}
	if got := s3.evaluate(t0.Add(2*time.Minute), 0, false, 4); len(got) != 0 {
		t.Errorf("5 分钟抑制窗口内不该重复，实际 %+v", got)
	}
	if got := s3.evaluate(t0.Add(6*time.Minute), 0, false, 4); len(got) != 1 {
		t.Errorf("过了 5 分钟应再次触发，实际 %+v", got)
	}
}

// TestChainDepth 钉住「阻塞链深度」的算法，含环路不死循环。
func TestChainDepth(t *testing.T) {
	cases := []struct {
		name  string
		items []driver.LockWait
		want  int
	}{
		{"没有等待", nil, 0},
		{"一条边", []driver.LockWait{{WaitingID: "5", BlockingID: "4"}}, 1},
		{"两级链", []driver.LockWait{
			{WaitingID: "5", BlockingID: "4"},
			{WaitingID: "6", BlockingID: "5"},
		}, 2},
		{"同一个持有者挡三个", []driver.LockWait{
			{WaitingID: "5", BlockingID: "4"},
			{WaitingID: "6", BlockingID: "4"},
			{WaitingID: "7", BlockingID: "4"},
		}, 1},
		{"纯环（没有根）", []driver.LockWait{
			{WaitingID: "5", BlockingID: "4"},
			{WaitingID: "4", BlockingID: "5"},
		}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := chainDepth(c.items); got != c.want {
				t.Errorf("chainDepth = %d, 期望 %d", got, c.want)
			}
		})
	}
}
