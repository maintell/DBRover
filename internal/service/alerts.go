package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

// Alert 是一条本地告警（FR-MON-08）。
type Alert struct {
	Rule    string    `json:"rule"`
	Level   string    `json:"level"`
	Detail  string    `json:"detail"`
	FiredAt time.Time `json:"fired_at"`
}

// alertState 记住「连续几次」与「上次触发时间」。
//
// 状态放内存、不落盘：docs/09 §7 写明告警**只在应用运行时生效**，v1 不做外部推送。
// 落盘会让人以为关掉应用之后它还在盯着——那是最危险的一种错觉。
type alertState struct {
	mu      sync.Mutex
	connHit int
	last    map[string]time.Time
}

// 抑制窗口（docs/09 §7）。
const (
	alertConnHold = 30 * time.Minute
	alertChainGo  = 5 * time.Minute
)

// evaluate 按规则算出本次该发的告警。纯函数（时间由参数给），便于单测。
func (s *alertState) evaluate(now time.Time, connPct float64, connKnown bool, chainDepth int) []Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[string]time.Time{}
	}
	out := []Alert{}
	fire := func(rule string, hold time.Duration, level, detail string) {
		if at, ok := s.last[rule]; ok && now.Sub(at) < hold {
			return
		}
		s.last[rule] = now
		out = append(out, Alert{Rule: rule, Level: level, Detail: detail, FiredAt: now})
	}

	// 规则 1：连接占用连续 3 次超过 80%。单次尖峰不该叫醒人。
	if connKnown && connPct > 80 {
		s.connHit++
	} else {
		s.connHit = 0
	}
	if s.connHit >= 3 {
		fire("连接占用", alertConnHold, "warn",
			fmt.Sprintf("连接占用连续 %d 次采样超过 80%%（本次 %.0f%%）", s.connHit, connPct))
	}

	// 规则 2：阻塞链深度 > 3。
	if chainDepth > 3 {
		fire("阻塞链", alertChainGo, "warn", fmt.Sprintf("阻塞链深度 %d（阈值 3）", chainDepth))
	}
	return out
}

// Alerts 评估一次告警（FR-MON-08）。
func (e *Executor) Alerts(ctx context.Context) ([]Alert, error) {
	var pct float64
	known := false
	if e.conn.Capabilities().Metrics {
		m, err := e.Metrics(ctx)
		if err == nil && m != nil && m.MaxConnections > 0 && m.Degraded == "" {
			pct = float64(m.Connections) / float64(m.MaxConnections) * 100
			known = true
		}
	}
	depth := 0
	if e.conn.Capabilities().LockGraph {
		if locks, err := e.LockWaits(ctx); err == nil {
			depth = chainDepth(locks)
		}
	}
	return e.alerts.evaluate(time.Now(), pct, known, depth), nil
}

// chainDepth 算阻塞链的最大深度：从没有在等别人的持有者往下走最长的一条。
//
// 环路（A 等 B、B 等 A）用访问集合挡住，否则会转死——真实环境里它会发生。
func chainDepth(items []driver.LockWait) int {
	children := map[string][]string{}
	waiting := map[string]bool{}
	for _, it := range items {
		children[it.BlockingID] = append(children[it.BlockingID], it.WaitingID)
		waiting[it.WaitingID] = true
	}
	var walk func(id string, seen map[string]bool) int
	walk = func(id string, seen map[string]bool) int {
		best := 0
		for _, ch := range children[id] {
			if seen[ch] {
				continue
			}
			seen[ch] = true
			if d := 1 + walk(ch, seen); d > best {
				best = d
			}
			delete(seen, ch)
		}
		return best
	}
	best := 0
	for id := range children {
		if waiting[id] {
			continue // 自己在等别人的不是根
		}
		if d := walk(id, map[string]bool{id: true}); d > best {
			best = d
		}
	}
	return best
}
