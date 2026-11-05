package api

import "net/http"

// handleAlerts 评估一次本地告警（FR-MON-08）。
//
// 说明随响应一起给：告警**只在应用运行时生效**，关掉应用就不再盯着——不说清楚，
// 用户会以为它是个常驻的监控。
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	if _, isRel := e.relational(); !isRel {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个 key-value 连接，没有这些告警规则", "", false)
		return
	}
	items, err := e.exec.Alerts(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "DB_METADATA_FAILED", "connection", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"note":  "告警只在应用运行时生效（面板打开才会评估），v1 不做外部推送",
	})
}
