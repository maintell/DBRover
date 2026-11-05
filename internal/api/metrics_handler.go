package api

import (
	"fmt"
	"net/http"
)

// handleMetrics 给出性能面板的「连接与负载」「缓存命中」两张卡（FR-MON-01）。
//
// 采集超时不当错误：服务层返回 Degraded 的摘要，这里照样 200。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	drv, isRel := e.relational()
	if !isRel {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个 key-value 连接，没有性能指标", "", false)
		return
	}
	if !drv.Capabilities().Metrics {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			fmt.Sprintf("%s 没有性能指标（当前只做了 PostgreSQL 与 MySQL）", drv.Engine()), "", false)
		return
	}
	sum, err := e.exec.Metrics(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "DB_METADATA_FAILED", "connection", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}
