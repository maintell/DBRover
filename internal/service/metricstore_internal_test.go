package service

import (
	"path/filepath"
	"testing"
	"time"
)

// TestMetricStoreRecordAndSeries 钉住 ADR-0016 的三条：按分钟归并、按窗口取、7 天清理。
func TestMetricStoreRecordAndSeries(t *testing.T) {
	st, err := OpenMetricStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	defer func() { _ = st.Close() }()

	now := time.Unix(1793840000, 0)
	// 同一分钟写两次：只留最后一条（面板每 5 秒轮询，不归并一小时就是 720 行）。
	if err := st.Record("c1", now, map[string]float64{"qps": 1.5}); err != nil {
		t.Fatalf("写入: %v", err)
	}
	if err := st.Record("c1", now.Add(20*time.Second), map[string]float64{"qps": 2.5}); err != nil {
		t.Fatalf("写入: %v", err)
	}
	pts, err := st.Series("c1", "qps", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("同一分钟应只有一行，实际 %d 行", len(pts))
	}
	if pts[0].Value != 2.5 {
		t.Errorf("应保留最后一次写入（2.5），实际 %v", pts[0].Value)
	}

	// 不同连接互不串：Series 按 connection_id 过滤。
	if err := st.Record("c2", now, map[string]float64{"qps": 9}); err != nil {
		t.Fatalf("写入: %v", err)
	}
	if pts, _ := st.Series("c1", "qps", now.Add(-time.Minute)); len(pts) != 1 {
		t.Errorf("别的连接的采样不该混进来，实际 %d 行", len(pts))
	}

	// 保留期：8 天前的点在「两天后再写一次」时应被清掉（prune 每天最多一次）。
	old := now.Add(-8 * 24 * time.Hour)
	if err := st.Record("c3", old, map[string]float64{"qps": 7}); err != nil {
		t.Fatalf("写入旧点: %v", err)
	}
	if err := st.Record("c3", now.Add(48*time.Hour), map[string]float64{"qps": 8}); err != nil {
		t.Fatalf("写入新点: %v", err)
	}
	pts, err = st.Series("c3", "qps", old.Add(-time.Hour))
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if len(pts) != 1 || pts[0].Value != 8 {
		t.Errorf("超过 7 天的点应被清掉，只剩新点，实际 %+v", pts)
	}
}
