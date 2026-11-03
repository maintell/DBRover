package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/maintell/DBRover/internal/service"
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

// explainRequest 是取执行计划的请求体。
type explainRequest struct {
	SQL string `json:"sql"`
	// Cursor 与 /exec 同义：给了就只解释光标所在的那一条。
	Cursor *int `json:"cursor"`
}

// handleExplain 返回一条查询的执行计划（不执行语句本身）。
//
// 只给计划不执行：EXPLAIN ANALYZE 会真跑一遍，那种包装不放在这里——
// 想看真实耗时应该直接执行，而不是打着「看计划」的名义跑一条写语句。
func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	if _, okRel := e.relational(); !okRel {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"key-value 连接没有执行计划", "", false)
		return
	}
	var req explainRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}
	if strings.TrimSpace(req.SQL) == "" {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", "sql 不能为空", "", false)
		return
	}

	out, err := e.exec.Explain(r.Context(), service.Request{
		SQL:    req.SQL,
		Cursor: req.Cursor,
		Actor:  "human:http",
	})
	if err != nil {
		if errors.Is(err, service.ErrExplainUnsupported) {
			writeError(w, http.StatusBadRequest, "EXPLAIN_UNSUPPORTED", "internal",
				err.Error(), "MSSQL 需先开 SET SHOWPLAN_ALL，Oracle 需 EXPLAIN PLAN FOR 再查 PLAN_TABLE", false)
			return
		}
		writeError(w, http.StatusBadGateway, "SQL_EXEC_FAILED", "connection", err.Error(), "", false)
		return
	}
	// 把计划翻成「哪里可能不对」——只翻译引擎自己说的话（SCAN / Seq Scan / type=ALL），
	// 不自己估代价：跨引擎自算的代价必然是错的。
	var findings []service.PlanFinding
	if len(out.Results) > 0 {
		findings = service.AnalyzePlan(e.conn.Engine, out.Results[0].Columns, out.Results[0].Rows)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"execution_id": out.ExecutionID,
		"risk":         out.Risk,
		"results":      out.Results,
		"findings":     findings,
		"duration_ms":  out.Duration.Milliseconds(),
		"error":        out.Err,
	})
}

// auditVerifier 是「能自证完整性」的审计实现（docs/10 §6.2）。
//
// 用可选接口而不是往 AuditSink 上加方法：内存实现与测试替身不需要背这个能力，
// 加进去它们就得提供一堆空实现。
type auditVerifier interface {
	Verify() (service.VerifyResult, error)
}

// handleAuditVerify 校验审计链的完整性。
func (s *Server) handleAuditVerify(w http.ResponseWriter, r *http.Request) {
	if s.audit == nil {
		writeJSON(w, http.StatusOK, map[string]any{"supported": false, "reason": "没有启用审计"})
		return
	}
	v, okVerifier := s.audit.(auditVerifier)
	if !okVerifier {
		writeJSON(w, http.StatusOK, map[string]any{
			"supported": false,
			"reason":    "当前审计实现不落盘（内存实现），没有可校验的链条",
		})
		return
	}
	res, err := v.Verify()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "AUDIT_VERIFY_FAILED", "internal", err.Error(), "", false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"supported":      true,
		"ok":             res.OK,
		"events":         res.Events,
		"broken_at":      res.BrokenAt,
		"reason":         res.Reason,
		"anchor_checked": res.AnchorChecked,
		"anchor_ok":      res.AnchorOK,
	})
}
