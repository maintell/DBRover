package service

import (
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestSplitBlockers 锁住「同样是阻塞项，有的挡有的不挡」这条规则。
//
// 写错的两种后果都很具体：把长事务也算成阻断，忙碌的库上工具就完全没法用；
// 把锁等待当成提示，则会在队列里继续加一条拿锁的 DDL。
func TestSplitBlockers(t *testing.T) {
	blockers := []model.Blocker{
		{Kind: "idle_in_transaction", Detail: "事务 A 已持续 90 秒"},
		{Kind: "lock_wait", Detail: "会话 7 正在等锁 12 秒"},
		{Kind: "idle_in_transaction", Detail: "事务 B 已持续 70 秒"},
		{Kind: "idle_in_transaction", Detail: "事务 C 已持续 65 秒"},
		{Kind: "idle_in_transaction", Detail: "事务 D 已持续 61 秒"},
	}
	waits, notes := splitBlockers(blockers, 3)
	if len(waits) != 1 || !strings.Contains(waits[0], "等锁") {
		t.Fatalf("锁等待必须单独成堆: %+v", waits)
	}
	// 3 条具体 + 1 条「另有 N 个」。
	if len(notes) != 4 {
		t.Fatalf("长事务提示条数 = %d, 期望 3 条具体 + 1 条汇总: %+v", len(notes), notes)
	}
	if !strings.Contains(notes[3], "另有 1 个") {
		t.Errorf("应汇总剩余数量: %+v", notes)
	}
	for _, n := range notes[:3] {
		if strings.Contains(n, "等锁") {
			t.Errorf("锁等待不该出现在提示里: %q", n)
		}
	}

	// 只有长事务：不阻断。
	if w, _ := splitBlockers([]model.Blocker{{Kind: "idle_in_transaction", Detail: "x"}}, 3); len(w) != 0 {
		t.Errorf("长事务不该阻断, 实际 %+v", w)
	}
	// 空输入：两堆都空。
	if w, n := splitBlockers(nil, 3); len(w) != 0 || len(n) != 0 {
		t.Errorf("空输入不该产生任何东西: %v / %v", w, n)
	}
}
