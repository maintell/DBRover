package api

import (
	"fmt"
	"net/http"
	"strconv"
)

// handleSlowQueries 给出慢查询 Top N（FR-MON-04）。
//
// 两条降级路径都返回 200：实例没开慢查询统计（available=false + hint），以及
// 采集超时（degraded 非空）。它们都不是「服务出错」，不该让面板整块红掉。
func (s *Server) handleSlowQueries(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	drv, isRel := e.relational()
	if !isRel {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个 key-value 连接，没有慢查询统计", "", false)
		return
	}
	if !drv.Capabilities().SlowQueryStats {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			fmt.Sprintf("%s 没有慢查询统计（当前只做了 PostgreSQL 与 MySQL）", drv.Engine()), "", false)
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 200 {
			writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
				"limit 必须是 1..200 的整数", "", false)
			return
		}
		limit = n
	}
	res, err := e.exec.SlowQueries(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, "DB_METADATA_FAILED", "connection", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
