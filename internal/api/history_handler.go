package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/maintell/DBRover/internal/service"
)

// UseMetricStore 打开本地指标留存（ADR-0016）。
//
// 用 setter 而不是改 New 的签名：New 在几十个测试里被调用，为一个可选能力加参数
// 会把噪音撒得到处都是。
func (s *Server) UseMetricStore(st *service.MetricStore) { s.metrics = st }

// handleHistory 给某个指标的历史点（FR-MON-05）。
//
// 没开留存时返回 200 + available=false：这不是错误，是「启动时没指定 -metrics」。
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "qps"
	}
	minutes := 60
	if raw := r.URL.Query().Get("minutes"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 7*24*60 {
			writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
				"minutes 必须是 1..10080 的整数", "", false)
			return
		}
		minutes = n
	}
	if s.metrics == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"hint":      "启动时加 -metrics <文件> 才会留存趋势（面板打开时才采样，最多保留 7 天）",
		})
		return
	}
	pts, err := s.metrics.Series(e.conn.ID, metric, time.Now().Add(-time.Duration(minutes)*time.Minute))
	if err != nil {
		writeError(w, http.StatusBadGateway, "METRIC_HISTORY_FAILED", "internal", err.Error(), "", false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true, "metric": metric, "minutes": minutes, "points": pts,
	})
}
