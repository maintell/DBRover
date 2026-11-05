package api

import (
	"net/http"
)

// handleHealthCheck 跑一次巡检并出报告（FR-MON-07）。
//
// 单项采集失败不影响整份报告：那一项记 unknown 并写明原因，其余照常给结论。
func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	if _, isRel := e.relational(); !isRel {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个 key-value 连接，没有关系库的巡检项", "", false)
		return
	}
	report, err := e.exec.HealthCheck(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "DB_METADATA_FAILED", "connection", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
