package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/maintell/DBRover/internal/service"
)

// handleLockWaits 给出锁等待的「谁在等谁」（FR-MON-03）。
//
// 没有等待时返回空数组而不是 404：那正是健康状态，不该被当成「查不到」。
func (s *Server) handleLockWaits(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	drv, isRel := e.relational()
	if !isRel {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个 key-value 连接，没有锁", "", false)
		return
	}
	if !drv.Capabilities().LockGraph {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			fmt.Sprintf("%s 没有锁等待视图（当前只做了 PostgreSQL 与 MySQL）", drv.Engine()), "", false)
		return
	}
	items, err := e.exec.LockWaits(r.Context())
	if errors.Is(err, service.ErrCollectTimeout) {
		// 超时不算错误（docs/09 §2.1）；空列表 + degraded 说明，而不是 502。
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "degraded": err.Error()})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "DB_METADATA_FAILED", "connection", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
