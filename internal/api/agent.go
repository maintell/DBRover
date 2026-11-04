package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/maintell/DBRover/internal/agent"
	"github.com/maintell/DBRover/internal/service"
)

// AgentConfig 是模型服务的连接参数。
//
// 目前从进程环境变量读（见 cmd/dbrover）：API Key 走环境变量是过渡做法，
// 与「凭据存 OS 凭据库」的目标有距离，已在 docs/11 §8.3 标注为已知限制。
type AgentConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	// ContextTokens 是模型上下文窗口的 token 数；>0 时长会话按「估算 token 达到
	// 窗口 85%」压缩，0 表示未知（退回字符预算）。
	ContextTokens int
}

// Configured 判断是否配了可用的模型服务。
func (c AgentConfig) Configured() bool {
	return c.BaseURL != "" && c.Model != ""
}

// SetAgentConfig 注入模型服务配置。空配置表示没配，Agent 端点会明确拒绝。
func (s *Server) SetAgentConfig(c AgentConfig) {
	s.mu.Lock()
	s.agentCfg = c
	s.mu.Unlock()
}

func (s *Server) agentConfig() AgentConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.agentCfg
}

// agentConv 是一次 Agent 会话。
//
// 会话持有自己的 Runtime：历史、已生成的预案、已批准的预案都是会话私有的。
// 共用一份 Runtime 会让 A 会话里批准过的预案在 B 会话里也能执行。
type agentConv struct {
	id       string
	connID   string
	rt       *agent.Runtime
	exec     *service.Executor
	mode     agent.Mode
	provider string
	created  time.Time
	lastUsed time.Time
	mu       sync.Mutex

	// runMu 只保护 cancel / running，刻意与 mu 分开：mu 在 Ask 的整轮里都是被持有的，
	// 中止请求若去抢 mu，就得等这一轮自己结束——那时中止已经毫无意义。
	runMu  sync.Mutex
	cancel context.CancelFunc
}

// agentConvTTL 是空闲会话的保留时长。会话里存着未执行的预案，留太久是负担。
const agentConvTTL = 30 * time.Minute

func (s *Server) newAgentConvID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return "conv-" + itoa(s.seq)
}

// purgeAgentConvs 清掉空闲超时的会话。
func (s *Server) purgeAgentConvs(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.agentConvs {
		if now.Sub(c.lastUsed) > agentConvTTL {
			delete(s.agentConvs, id)
		}
	}
}

// beginRun 记录这一轮的取消函数，返回的函数在结束时清空。
func (c *agentConv) beginRun(cancel context.CancelFunc) func() {
	c.runMu.Lock()
	c.cancel = cancel
	c.runMu.Unlock()
	return func() {
		c.runMu.Lock()
		c.cancel = nil
		c.runMu.Unlock()
	}
}

// Abort 取消正在跑的那一轮；没有在跑时返回 false。
//
// 返回 false 不是错误：界面可能点了两次，或者这一轮刚好结束。把「没停到」
// 和「停到了」分开报，才能避免界面显示一个假的「已停止」。
func (c *agentConv) Abort() bool {
	c.runMu.Lock()
	cancel := c.cancel
	c.runMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// Running 表示当前是否有一轮在跑。
func (c *agentConv) Running() bool {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	return c.cancel != nil
}

func (s *Server) agentConvByID(id string) (*agentConv, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.agentConvs[id]
	if ok {
		c.lastUsed = time.Now()
	}
	return c, ok
}

// handleAgentStatus 告诉界面「现在能不能用 Agent、用的是哪个模型」。
func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.agentConfig()
	out := map[string]any{
		"configured": cfg.Configured(),
		"model":      cfg.Model,
		"base_url":   cfg.BaseURL,
	}
	if !cfg.Configured() {
		out["hint"] = "未配置模型服务：设置 DBRover_AGENT_BASE_URL / DBRover_AGENT_MODEL / DBRover_AGENT_API_KEY 后重启 serve"
	}
	writeJSON(w, http.StatusOK, out)
}

// agentConvRequest 是新建会话的请求体。
type agentConvRequest struct {
	ConnectionID string `json:"connection_id"`
	// Mode 省略时用只读：默认最保守。
	Mode string `json:"mode"`
}

// handleAgentConversationCreate 新建会话。
func (s *Server) handleAgentConversationCreate(w http.ResponseWriter, r *http.Request) {
	var req agentConvRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}

	s.mu.RLock()
	e, okConn := s.conns[req.ConnectionID]
	s.mu.RUnlock()
	if !okConn {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	drv, okRel := e.relational()
	if !okRel {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"Agent 目前只支持关系型连接", "key-value 连接的浏览与改值请用 /kv 端点", false)
		return
	}

	cfg := s.agentConfig()
	if !cfg.Configured() {
		writeError(w, http.StatusBadRequest, "AGENT_NOT_CONFIGURED", "internal",
			"没有配置模型服务",
			"设置 DBRover_AGENT_BASE_URL / DBRover_AGENT_MODEL（以及需要时的 DBRover_AGENT_API_KEY）后重启 serve", false)
		return
	}

	mode, err := agent.ParseMode(req.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}

	prov, err := agent.NewOpenAI(agent.OpenAIConfig{
		BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Model: cfg.Model,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "AGENT_NOT_CONFIGURED", "internal", err.Error(), "", false)
		return
	}

	rt, err := agent.NewRuntime(agent.Options{
		Provider:    prov,
		Executor:    e.exec,
		Catalog:     service.NewCatalog(drv),
		Mode:        mode,
		Environment: e.conn.Environment,
		AccessMode:  e.conn.AccessMode,
		CatalogName: e.conn.Catalog,
		Schema:      e.conn.Schema,
		// 窗口来自配置（DBROVER_AGENT_CONTEXT_TOKENS）；没配就让运行时按字符预算，
		// 不猜一个假的窗口值。
		ContextTokens: cfg.ContextTokens,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "AGENT_NOT_CONFIGURED", "internal", err.Error(), "", false)
		return
	}

	now := time.Now()
	c := &agentConv{
		id: s.newAgentConvID(), connID: req.ConnectionID, rt: rt, exec: e.exec,
		mode: rt.Mode(), provider: rt.ProviderName(), created: now, lastUsed: now,
	}
	s.mu.Lock()
	s.purgeAgentConvsLocked(now)
	s.agentConvs[c.id] = c
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         c.id,
		"connection": req.ConnectionID,
		"mode":       string(c.mode),
		"provider":   c.provider,
		"tools":      len(rt.Tools()),
	})
}

// purgeAgentConvsLocked 与 purgeAgentConvs 同义，但要求调用方已持有写锁。
func (s *Server) purgeAgentConvsLocked(now time.Time) {
	for id, c := range s.agentConvs {
		if now.Sub(c.lastUsed) > agentConvTTL {
			delete(s.agentConvs, id)
		}
	}
}

// agentMessageRequest 是发一条消息的请求体。
type agentMessageRequest struct {
	Content string `json:"content"`
}

// pendingApproval 是等人类确认的预案，给界面渲染审批卡片用。
//
// 卡片必须展示**最终 SQL**，不是 Agent 的描述：描述可以被库里的数据操纵，SQL 不会。
type pendingApproval struct {
	PlanID       string                  `json:"plan_id"`
	Table        string                  `json:"table"`
	Risk         string                  `json:"risk"`
	Friction     string                  `json:"friction"`
	AffectedRows int64                   `json:"affected_rows"`
	Statements   []service.PlanStatement `json:"statements"`
	Rollback     []service.PlanStatement `json:"rollback"`
	Irreversible bool                    `json:"irreversible"`
}

// handleAgentMessage 发一条消息并等这一轮跑完。
//
// 同步返回而不是流式：一轮通常是几次工具调用，秒级到分钟级。超预算、循环、
// 失败上限这些「没跑完」的情况不是 HTTP 错误——它们要带着已完成的部分回答
// 一起返回，界面上才能说清「停在哪、为什么停」。
func (s *Server) handleAgentMessage(w http.ResponseWriter, r *http.Request) {
	c, okConv := s.agentConvByID(r.PathValue("id"))
	if !okConv {
		writeError(w, http.StatusNotFound, "CONVERSATION_NOT_FOUND", "internal", "会话不存在或已过期", "重新新建会话", false)
		return
	}
	var req agentMessageRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	// 登记取消句柄：/abort 靠它真的停下这一轮，而不是只改一个状态位。
	done := c.beginRun(cancel)
	defer done()

	// 流式：先把每个步骤写出去，最后再给结论与待批预案。
	if r.URL.Query().Get("stream") == "1" {
		s.streamAgentMessage(w, r, c, req.Content)
		return
	}

	c.mu.Lock()
	res, err := c.rt.Ask(ctx, req.Content)
	c.mu.Unlock()

	if err != nil && !softStop(err) {
		writeError(w, http.StatusBadGateway, "AGENT_PROVIDER_FAILED", "connection", err.Error(),
			"检查 base_url 是否可达、model 名是否存在", true)
		return
	}

	pending := make([]pendingApproval, 0, 4)
	for _, p := range c.rt.PendingPlans() {
		pending = append(pending, pendingApproval{
			PlanID: p.ID, Table: p.Table.Name, Risk: p.Risk.String(), Friction: p.Friction.String(),
			AffectedRows: p.AffectedRows, Statements: p.Statements, Rollback: p.Rollback,
			Irreversible: p.Irreversible,
		})
	}

	out := map[string]any{
		"answer":            res.Answer,
		"steps":             res.Steps,
		"notes":             res.Notes,
		"usage":             res.Usage,
		"stopped":           res.Stopped,
		"pending_approvals": pending,
	}
	if msg := stopMessage(err); msg != "" {
		out["stop_message"] = msg
	}
	writeJSON(w, http.StatusOK, out)
}

// agentApprovalRequest 是批准/拒绝的请求体。Approve 省略时视为批准。
type agentApprovalRequest struct {
	Approve *bool `json:"approve"`
}

// handleAgentApproval 由**人**批准或拒绝一张预案。
//
// 这是整个 Agent 安全模型里唯一的签字口：它不在工具面上，模型调不到。
func (s *Server) handleAgentApproval(w http.ResponseWriter, r *http.Request) {
	c, okConv := s.agentConvByID(r.PathValue("id"))
	if !okConv {
		writeError(w, http.StatusNotFound, "CONVERSATION_NOT_FOUND", "internal", "会话不存在或已过期", "重新新建会话", false)
		return
	}
	var req agentApprovalRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}
	approve := true
	if req.Approve != nil {
		approve = *req.Approve
	}
	planID := r.PathValue("pid")

	if approve {
		if err := c.rt.Approve(planID); err != nil {
			writeError(w, http.StatusNotFound, "PLAN_NOT_FOUND", "policy", err.Error(), "预案可能已执行或已过期", false)
			return
		}
	} else {
		c.rt.Reject(planID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan_id": planID, "approved": approve})
}

// softStop 判断错误是不是「有结论但没走完」。
//
// 这五种要返回 200：结论与已完成的步骤都要带回去，界面上才能说清「停在哪、为什么停」。
// 只有模型服务本身不可用才算 502——「人点了停止」不是故障。
func softStop(err error) bool {
	return errors.Is(err, agent.ErrStepBudget) ||
		errors.Is(err, agent.ErrLoopDetected) ||
		errors.Is(err, agent.ErrToolFailures) ||
		errors.Is(err, agent.ErrAborted) ||
		errors.Is(err, agent.ErrDuration)
}

func stopMessage(err error) string {
	switch {
	case errors.Is(err, agent.ErrStepBudget):
		return "达到最大步数，任务未完成；已完成的部分见步骤"
	case errors.Is(err, agent.ErrLoopDetected):
		return "检测到重复调用同一工具与参数，已中断"
	case errors.Is(err, agent.ErrToolFailures):
		return "同一工具反复失败，已停止重试"
	case errors.Is(err, agent.ErrAborted):
		return "已按请求中止；已执行的步骤仍然有效"
	case errors.Is(err, agent.ErrDuration):
		return "超过单轮时间上限，已停止"
	}
	return ""
}

// streamAgentMessage 用 NDJSON 把一轮问答边跑边写出去。
//
// 一条一行：{"kind":"step"|"answer"|"done"|"result"|"error", ...}。
// 为什么不用 SSE：NDJSON 用普通 fetch + ReadableStream 就能读，不需要 EventSource，
// 也不需要为「POST + 请求体」绕路。
//
// 一旦开始写就改不了状态码了：出错只能发一条 {"kind":"error"}，
// 与执行门的 NDJSON 进度流是同一套约束。
func (s *Server) streamAgentMessage(w http.ResponseWriter, r *http.Request, c *agentConv, content string) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	write := func(v any) {
		_ = enc.Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	done := c.beginRun(cancel)
	defer done()

	c.mu.Lock()
	res, err := c.rt.AskStream(ctx, content, func(ev agent.Event) { write(ev) })
	c.mu.Unlock()

	if err != nil && !softStop(err) {
		write(map[string]any{"kind": "error", "code": "AGENT_PROVIDER_FAILED", "message": err.Error()})
		return
	}

	pending := make([]pendingApproval, 0, 4)
	for _, p := range c.rt.PendingPlans() {
		pending = append(pending, pendingApproval{
			PlanID: p.ID, Table: p.Table.Name, Risk: p.Risk.String(), Friction: p.Friction.String(),
			AffectedRows: p.AffectedRows, Statements: p.Statements, Rollback: p.Rollback,
			Irreversible: p.Irreversible,
		})
	}
	final := map[string]any{
		"kind": "result", "answer": res.Answer, "stopped": res.Stopped,
		"pending_approvals": pending, "usage": res.Usage, "notes": res.Notes,
	}
	if msg := stopMessage(err); msg != "" {
		final["stop_message"] = msg
	}
	write(final)
}

// handleAgentExport 导出一份会话记录（步骤 + 结论，不含工具结果全文）。
func (s *Server) handleAgentExport(w http.ResponseWriter, r *http.Request) {
	c, okConv := s.agentConvByID(r.PathValue("id"))
	if !okConv {
		writeError(w, http.StatusNotFound, "CONVERSATION_NOT_FOUND", "internal", "会话不存在或已过期", "重新新建会话", false)
		return
	}
	writeJSON(w, http.StatusOK, c.rt.Export())
}

// handleAgentTools 列出 Agent 能做什么：名称、说明、参数 schema。
//
// 不需要连接也不需要模型配置：工具契约是静态的。这一点很重要——界面上
// 「这个 Agent 会做什么」不该依赖「现在有没有连上库」，否则一条干净的实例
// 永远显示空清单，用户只能去读文档。
func (s *Server) handleAgentTools(w http.ResponseWriter, _ *http.Request) {
	tools := agent.Catalog()
	writeJSON(w, http.StatusOK, map[string]any{
		"tools": tools,
		"count": len(tools),
	})
}

// handleAgentConversation 返回会话元信息 + 既有记录。
//
// 记录用 Export 的形状（provider / mode / turns / notes）：它刻意不含工具结果
// 全文，所以这个端点可以放心给界面轮询，不会把业务数据一遍遍搬来搬去。
func (s *Server) handleAgentConversation(w http.ResponseWriter, r *http.Request) {
	c, okConv := s.agentConvByID(r.PathValue("id"))
	if !okConv {
		writeError(w, http.StatusNotFound, "CONVERSATION_NOT_FOUND", "internal", "会话不存在或已过期", "重新新建会话", false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            c.id,
		"connection_id": c.connID,
		"mode":          string(c.mode),
		"provider":      c.provider,
		"created":       c.created,
		"last_used":     c.lastUsed,
		"running":       c.Running(),
		"transcript":    c.rt.Export(),
	})
}

// handleAgentAbort 请求中止正在跑的那一轮。
//
// 语义是「取消已发出」，不是「已经停了」：Ask 在下一个模型调用或工具边界看到
// ctx 取消才收尾，中间可能还差一次网络往返。所以返回 200 + aborted=true 表示
// 请求已送达，客户端应继续等那条 messages 请求自己返回（它会带上已完成的部分）。
//
// 没在跑的时候返回 409：把「什么都没停」明确说出来，界面才不会显示一个假的
// 「已停止」。
func (s *Server) handleAgentAbort(w http.ResponseWriter, r *http.Request) {
	c, okConv := s.agentConvByID(r.PathValue("id"))
	if !okConv {
		writeError(w, http.StatusNotFound, "CONVERSATION_NOT_FOUND", "internal", "会话不存在或已过期", "重新新建会话", false)
		return
	}
	if !c.Abort() {
		writeError(w, http.StatusConflict, "CONVERSATION_IDLE", "internal",
			"这个会话当前没有在跑的一轮", "无需中止；若界面仍显示运行中，等这一轮自己返回", false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"aborted": true,
		"note": "取消已发出：这一轮会在下一个边界收尾，已执行的步骤仍然保留"})
}
