package api

import (
	"net/http"
)

// handleCancel 终止这条连接上正在执行的语句（docs/05 验收 9）。
//
// 为什么要一个独立端点：取消是用户在看到「跑太久」之后的动作，而那条执行请求
// 正阻塞着等结果，没法用它自己来取消自己。
//
// canceled=false 不是错误——用户可能按晚了，或者语句刚好跑完。
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	if _, okRel := e.relational(); !okRel {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"key-value 连接没有可取消的长语句", "Redis / Mongo 的命令是毫秒级的，不需要取消", false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"canceled": e.exec.Cancel(r.Context())})
}
