package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/service"
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
	if errors.Is(err, service.ErrCollectTimeout) {
		// docs/09 §2.1：超时不算错误，记一次降级。
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []any{}, "growth_available": false, "degraded": err.Error(),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "DB_METADATA_FAILED", "connection", err.Error(), "", true)
		return
	}
	// 顺手把体量记进留存并算增长（FR-MON-06 的增长部分，ADR-0016 的存储）。
	// 增长只在**两个以上采样点**时给：一个点只能说明现在多大，编个 0 出来更糟。
	type sizeItem struct {
		driver.TableSize
		GrowthBytes *int64  `json:"growth_bytes,omitempty"`
		GrowthHours float64 `json:"growth_hours,omitempty"`
	}
	out := make([]sizeItem, 0, len(items))
	now := time.Now()
	for _, it := range items {
		// 键必须带 schema：同一连接里两个 schema 有同名表时，只用表名会把两条序列
		// 写成同一条（metric_sample 的主键是 connection_id + metric + ts）。
		key := service.MetricSizePrefix + it.Name
		if it.Schema != "" {
			key = service.MetricSizePrefix + it.Schema + "." + it.Name
		}
		if s.metrics != nil {
			_ = s.metrics.Record(e.conn.ID, now, map[string]float64{key: float64(it.TotalBytes)})
		}
		row := sizeItem{TableSize: it}
		if s.metrics != nil {
			if pts, serr := s.metrics.Series(e.conn.ID, key, now.Add(-7*24*time.Hour)); serr == nil {
				if delta, hours, ok := service.GrowthOf(pts, it.TotalBytes); ok {
					row.GrowthBytes = &delta
					row.GrowthHours = hours
				}
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": out, "growth_available": s.metrics != nil,
	})
}
