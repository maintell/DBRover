package service

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// metricRetention 是留存期（docs/09 §4）。
const metricRetention = 7 * 24 * time.Hour

// MetricPoint 是时序上的一个点。
type MetricPoint struct {
	TS    int64   `json:"ts"` // unix 秒，按分钟对齐
	Value float64 `json:"value"`
}

// MetricStore 是本地的指标留存（ADR-0016）。
//
// 只存「面板打开时读到的那几条」：用户不关心的时候不需要趋势，常驻采集会持续向
// 生产库发查询。这不是妥协，是 docs/09 §4 明写的取舍。
type MetricStore struct {
	db *sql.DB
	mu sync.Mutex
	// lastPrune 记住上次清理时间：每天最多清一次，别每次写都扫一遍表。
	lastPrune time.Time
}

// createSQL 建表。普通字符串而不是反引号 raw string：这段只有单引号，用不上
// raw string，而用代码生成源码时 raw string 的边界很容易被写进产物。
const createSQL = "CREATE TABLE IF NOT EXISTS metric_sample (" +
	"connection_id TEXT NOT NULL, " +
	"ts INTEGER NOT NULL, " +
	"metric TEXT NOT NULL, " +
	"value REAL NOT NULL, " +
	"PRIMARY KEY (connection_id, metric, ts)) WITHOUT ROWID"

// OpenMetricStore 打开（或创建）指标文件。
func OpenMetricStore(path string) (*MetricStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// 单进程内的低频写，一条连接就够；多开只会争同一把文件锁。
	db.SetMaxOpenConns(1)
	// 逐条执行：modernc 的 Exec 不接受一次传两条 PRAGMA。
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA user_version=1"} {
		if _, err := db.Exec(q); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(createSQL); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &MetricStore{db: db}, nil
}

// Close 关闭文件。
func (s *MetricStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Record 写一条采样（同一分钟覆盖）。
//
// 按分钟对齐是关键：面板每 5 秒轮询一次，不归并的话一小时就是 720 行，而真正
// 有用的分辨率是分钟。
func (s *MetricStore) Record(connID string, at time.Time, values map[string]float64) error {
	if s == nil || s.db == nil || len(values) == 0 {
		return nil
	}
	ts := at.Unix() / 60 * 60
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare("INSERT OR REPLACE INTO metric_sample (connection_id, ts, metric, value) VALUES (?, ?, ?, ?)")
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer func() { _ = stmt.Close() }()
	for name, v := range values {
		if _, err := stmt.Exec(connID, ts, name, v); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.pruneLocked(at)
}

// pruneLocked 每天最多清理一次。调用方已持锁。
func (s *MetricStore) pruneLocked(now time.Time) error {
	if !s.lastPrune.IsZero() && now.Sub(s.lastPrune) < 24*time.Hour {
		return nil
	}
	s.lastPrune = now
	_, err := s.db.Exec("DELETE FROM metric_sample WHERE ts < ?", now.Add(-metricRetention).Unix())
	return err
}

// Series 取某个指标在 since 之后的点，按时间升序。
func (s *MetricStore) Series(connID, metric string, since time.Time) ([]MetricPoint, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("未开启指标留存（启动时加 -metrics <文件>）")
	}
	if metric == "" {
		return nil, fmt.Errorf("metric 不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query("SELECT ts, value FROM metric_sample WHERE connection_id = ? AND metric = ? AND ts >= ? ORDER BY ts",
		connID, metric, since.Unix())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []MetricPoint
	for rows.Next() {
		var p MetricPoint
		if err := rows.Scan(&p.TS, &p.Value); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
