package api

import (
	"fmt"
	"net/http"
	"strconv"
)

// handleTableSizes 给出表体量 Top N（FR-MON-06）。
func (s *Server) handleTableSizes(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	drv, isRel := e.relational()
	if !isRel {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个 key-value 连接，没有表体量", "", false)
		return
	}
	if !drv.Capabilities().Capacity {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			fmt.Sprintf("%s 没有表体量视图（当前只做了 PostgreSQL 与 MySQL）", drv.Engine()), "", false)
		return
	}
	limit := 10
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 200 {
			writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", "limit 必须是 1..200 的整数", "", false)
			return
		}
		limit = n
	}
	items, err := e.exec.Capacity(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, "DB_METADATA_FAILED", "connection", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
