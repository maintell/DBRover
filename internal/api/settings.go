package api

import (
	"net/http"

	"github.com/maintell/DBRover/internal/infra/settings"
)

// handleSettingsGet 返回当前设置。api_key_configured 只报有无，绝不回显值。
func (s *Server) handleSettingsGet(w http.ResponseWriter, _ *http.Request) {
	st := s.settingsStore()
	if st == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal",
			"设置存储未装配", "", false)
		return
	}
	out, err := st.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal", err.Error(), "", false)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSettingsPatch 局部更新设置。校验错误映射成 400 REQUEST_INVALID。
func (s *Server) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	st := s.settingsStore()
	if st == nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal",
			"设置存储未装配", "", false)
		return
	}
	var p settings.Patch
	if err := decode(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}
	out, err := st.Update(p)
	if err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}
	// 设置变了，重建 Agent 配置作用于下一次会话（hook 由 host 注入，避免 api→host 反向依赖）。
	if hook := s.settingsChangedHook(); hook != nil {
		hook()
	}
	writeJSON(w, http.StatusOK, out)
}
