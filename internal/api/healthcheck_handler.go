package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/maintell/DBRover/internal/service"
)

// handleHealthCheck 跑一次巡检并出报告（FR-MON-07）。
//
// `?format=markdown` 返回可直接贴进工单/群消息的表格（验收 12 的「报告可导出」）。
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
	if r.URL.Query().Get("format") == "markdown" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(healthMarkdown(report)))
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// healthMarkdown 把报告写成 Markdown 表格。
//
// 导出的意义是「能贴进工单或群里」：所以级别用中文而不是 ok/warn/crit，
// 且不夹任何只有本工具才懂的字段名。
func healthMarkdown(r *service.HealthReport) string {
	label := map[string]string{"ok": "正常", "warn": "注意", "crit": "有问题", "unknown": "未知"}
	var b strings.Builder
	b.WriteString("### 数据库体检报告\n\n")
	fmt.Fprintf(&b, "整体：**%s**（%s）\n\n", label[r.Level], r.GeneratedAt.Format("2006-01-02 15:04:05"))
	b.WriteString("| 项目 | 级别 | 结论 |\n|---|---|---|\n")
	for _, it := range r.Items {
		// 表格里的竖线与换行必须转义，否则一条语句就能把整张表拆坏。
		detail := strings.ReplaceAll(it.Detail, "|", "\\|")
		detail = strings.ReplaceAll(detail, "\n", " ")
		fmt.Fprintf(&b, "| %s | %s | %s |\n", it.Name, label[it.Level], detail)
	}
	return b.String()
}
