// Package api 是 HTTP 层，只做协议转换：校验、路由、错误映射、DTO 编解码。
//
// 本包不实现业务，一切能力经 service 触达。
package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
	"github.com/maintell/DBRover/internal/service"
	"github.com/maintell/DBRover/internal/webui"
)

// Opener 按引擎创建并打开连接器。由调用方（cmd）注入，避免 api 直接依赖具体驱动。
type Opener func(engine model.Engine, cfg driver.ConnectConfig) (driver.Connector, error)

// Server 是回环 HTTP 服务。
type Server struct {
	token  string
	opener Opener
	audit  service.AuditSink

	mu    sync.RWMutex
	conns map[string]*entry
	// cursorOwner 把游标 id 映射到所属连接，供 /results/{cid} 路由。
	cursorOwner map[string]*entry
	// plans 按 id 保管变更预案。
	// 只存服务端生成的预案：客户端不能回传预案，否则等于绕过风险分级。
	plans map[string]*storedPlan
	seq   int
}

type storedPlan struct {
	plan *service.ChangePlan
	exec *service.Executor
	exp  time.Time
}

type entry struct {
	conn model.Connection
	drv  driver.Connector
	exec *service.Executor
}

// New 创建服务。token 为空时随机生成。
func New(token string, opener Opener, audit service.AuditSink) *Server {
	if token == "" {
		token = randomToken()
	}
	return &Server{token: token, opener: opener, audit: audit,
		conns: map[string]*entry{}, cursorOwner: map[string]*entry{}, plans: map[string]*storedPlan{}}
}

// Token 返回本次进程使用的访问令牌。
func (s *Server) Token() string { return s.token }

// Close 关闭全部连接与游标。
func (s *Server) Close() {
	s.mu.Lock()
	conns := s.conns
	s.conns = map[string]*entry{}
	s.cursorOwner = map[string]*entry{}
	s.mu.Unlock()
	for _, e := range conns {
		e.exec.Close()
		_ = e.drv.Close()
	}
}

// route 是一条路由声明。路由表是唯一注册来源，
// 契约文件（openapi.json）必须与它逐条对应，由 TestSpecMatchesRoutes 强制。
type route struct {
	Method  string
	Path    string
	Public  bool // 免 token
	Handler http.HandlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{http.MethodGet, "/api/v1/health", true, s.handlePing},
		{http.MethodGet, "/api/v1/openapi.json", true, s.handleSpec},
		{http.MethodPost, "/api/v1/connections", false, s.handleCreateConnection},
		{http.MethodGet, "/api/v1/connections", false, s.handleListConnections},
		{http.MethodDelete, "/api/v1/connections/{id}", false, s.handleDeleteConnection},
		{http.MethodGet, "/api/v1/connections/{id}/health", false, s.handleConnHealth},
		{http.MethodGet, "/api/v1/connections/{id}/tables", false, s.handleTables},
		{http.MethodGet, "/api/v1/connections/{id}/tables/{name}/rows", false, s.handleTableRows},
		{http.MethodPost, "/api/v1/connections/{id}/exec", false, s.handleExec},
		{http.MethodPost, "/api/v1/connections/{id}/changes/plan", false, s.handleChangePlan},
		{http.MethodPost, "/api/v1/changes/{pid}/apply", false, s.handleChangeApply},
		{http.MethodPost, "/api/v1/connections/{id}/query", false, s.handleQuery},
		{http.MethodGet, "/api/v1/results/{cid}/page", false, s.handlePage},
		{http.MethodDelete, "/api/v1/results/{cid}", false, s.handleCloseCursor},
		{http.MethodGet, "/api/v1/audit", false, s.handleAudit},
	}
}

// Handler 返回带鉴权的处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	public := map[string]bool{}
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.Method+" "+rt.Path, rt.Handler)
		if rt.Public {
			public[rt.Method+" "+rt.Path] = true
		}
	}

	// 静态资源与入口页免鉴权：页面本身不含任何数据，数据接口仍需令牌。
	mux.HandleFunc("GET /app.js", webui.Asset("app.js"))
	mux.HandleFunc("GET /style.css", webui.Asset("style.css"))
	// 未知的 /api 路径必须返回 404，不能被 SPA 兜底吞成 200 的 HTML。
	mux.HandleFunc("/api/", s.handleAPINotFound)
	// SPA 兜底：注册为「任意方法」，否则会与更具体的 /api/ 模式冲突。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		webui.Index(w, r)
	})

	return s.withAuth(mux, public)
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "ROUTE_NOT_FOUND", "internal",
		"未知的接口路径: "+r.URL.Path, "参见 /api/v1/openapi.json", false)
}

func (s *Server) withAuth(next http.Handler, public map[string]bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 非 /api/ 路径是静态资源与 SPA 入口，不校验令牌。
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if public[r.Method+" "+r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-DBRover-Token")
		if got == "" || got != s.token {
			writeError(w, http.StatusUnauthorized, "AUTH_TOKEN_INVALID", "auth",
				"缺少或错误的访问令牌", "启动时输出的 token 需要放入 X-DBRover-Token 头", false)
			return
		}
		// Token 之外再校验来源：token 可能被带进浏览器上下文，
		// 那样任意网页都能发起跨站请求（docs/10 威胁模型 T2）。
		if !s.originAllowed(r) {
			writeError(w, http.StatusForbidden, "ORIGIN_NOT_ALLOWED", "auth",
				"请求来源不被允许", "只接受同源请求，或不带 Origin 的非浏览器客户端", false)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed 只接受「不带 Origin」（curl/桌面壳）或「同源」。
// 刻意不接受 "null"：它来自 file:// 或沙箱 iframe，属于不可信来源。
func (s *Server) originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

func (s *Server) handleSpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(SpecJSON())
}

// ---- DTO ----

type createConnectionRequest struct {
	Name    string `json:"name"`
	Engine  string `json:"engine"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Catalog string `json:"catalog"`
	User    string `json:"user"`
	// Password 只进不出：请求里出现一次，之后任何响应都不回显。
	Password    string `json:"password"`
	Environment string `json:"environment"`
	AccessMode  string `json:"access_mode"`
}

type connectionDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Engine      string `json:"engine"`
	Environment string `json:"environment"`
	AccessMode  string `json:"access_mode"`
}

type execRequest struct {
	SQL string `json:"sql"`
	// ApprovalToken 是服务端签发的审批凭证，绑定语句指纹与有效期。
	// 客户端不能用布尔值绕过 —— 审批的是「这段 SQL」，不是一个请求。
	ApprovalToken string `json:"approval_token"`
	MaxRows       int    `json:"max_rows"`
}

type execResponse struct {
	ExecutionID string                    `json:"execution_id"`
	Risk        string                    `json:"risk"`
	Friction    string                    `json:"friction"`
	DurationMS  int64                     `json:"duration_ms"`
	Results     []service.StatementResult `json:"results"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Category  string `json:"category"`
	Message   string `json:"message"`
	Hint      string `json:"hint,omitempty"`
	Retryable bool   `json:"retryable"`
	RequestID string `json:"request_id,omitempty"`
	// Approval 只在 POLICY_APPROVAL_REQUIRED 时出现，携带绑定语句指纹的凭证。
	Approval *approvalInfo `json:"approval,omitempty"`
}

type approvalInfo struct {
	StatementHash string `json:"statement_hash"`
	ExpiresAt     string `json:"expires_at"`
	Token         string `json:"token"`
}

// approvalTTL 是审批凭证有效期，与 docs/10 §5.1 一致。
const approvalTTL = 10 * time.Minute

// approvalToken 用进程令牌做 HMAC，签发「绑定语句指纹 + 过期时间」的凭证。
// 无状态：不存表，靠签名自证。
func (s *Server) approvalToken(hash string, exp time.Time) string {
	expStr := strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(s.token))
	mac.Write([]byte(hash + "|" + expStr))
	return expStr + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifyApproval 校验凭证签名、有效期与绑定内容。
func (s *Server) verifyApproval(tok, hash string) bool {
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 || hash == "" {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.token))
	mac.Write([]byte(hash + "|" + parts[0]))
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(parts[1]))
}

// ---- Handlers ----

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now().UTC()})
}

func (s *Server) handleCreateConnection(w http.ResponseWriter, r *http.Request) {
	var req createConnectionRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}
	if strings.TrimSpace(req.Engine) == "" {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", "engine 必填", "", false)
		return
	}

	env := model.ParseEnvironment(req.Environment)
	// 访问模式必须严格校验：拼错的值（如 read_only）如果被当成未知模式放行，
	// 只读守卫与驱动的只读模式会同时失效。
	mode, ok := model.ParseAccessMode(req.AccessMode)
	if !ok {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
			"access_mode 非法: "+req.AccessMode,
			"可选值: read-only / read-write / admin", false)
		return
	}
	if mode == "" {
		mode = model.DefaultAccessMode(env)
	}

	cfg := driver.ConnectConfig{
		Name:        req.Name,
		Host:        req.Host,
		Port:        req.Port,
		Catalog:     req.Catalog,
		Schema:      req.Catalog,
		User:        req.User,
		Password:    req.Password,
		AccessMode:  mode,
		Environment: env,
	}
	if req.Engine == string(model.EngineSQLite) {
		// SQLite 的 Host 就是文件路径，以 -db 语义传入。
		cfg.Host = req.Host
	}
	engine := model.Engine(req.Engine)

	conn, err := s.opener(engine, cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, "DB_CONNECT_FAILED", "connection", err.Error(),
			"检查文件路径与访问模式", false)
		return
	}

	s.mu.Lock()
	s.seq++
	id := "conn-" + itoa(s.seq)
	e := &entry{
		conn: model.Connection{
			ID: id, Name: req.Name, Engine: engine, Host: req.Host,
			Environment: env, AccessMode: mode,
		},
		drv:  conn,
		exec: service.NewExecutor(conn, env, mode, s.audit),
	}
	s.conns[id] = e
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, connectionDTO{
		ID: id, Name: req.Name, Engine: string(engine),
		Environment: string(env), AccessMode: string(mode),
	})
}

func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]connectionDTO, 0, len(s.conns))
	for _, e := range s.conns {
		items = append(items, connectionDTO{
			ID: e.conn.ID, Name: e.conn.Name, Engine: string(e.conn.Engine),
			Environment: string(e.conn.Environment), AccessMode: string(e.conn.AccessMode),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	e, ok := s.conns[id]
	delete(s.conns, id)
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	e.exec.Close()
	_ = e.drv.Close()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConnHealth(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	h, err := e.drv.Health(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "DB_HEALTH_FAILED", "connection", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     h.Version,
		"read_only":   h.ReadOnly,
		"environment": e.conn.Environment,
		"identity":    h.Identity,
		"warnings":    h.Warnings,
	})
}

func (s *Server) handleTables(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	objs, err := e.drv.Meta().Objects(r.Context(), "", "", model.KindTable, model.KindView)
	if err != nil {
		writeError(w, http.StatusBadGateway, "DB_METADATA_FAILED", "connection", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": objs})
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	var req execRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}

	// 先算本次语句的指纹，再校验审批凭证是否绑定了它。
	hash, _ := service.Fingerprint(e.conn.Engine, req.SQL)
	approved := false
	if req.ApprovalToken != "" {
		if !s.verifyApproval(req.ApprovalToken, hash) {
			writeError(w, http.StatusConflict, "POLICY_APPROVAL_MISMATCH", "policy",
				"审批凭证无效、已过期，或与当前语句不匹配",
				"重新提交以获取新的审批凭证", false)
			return
		}
		approved = true
	}

	out, err := e.exec.Execute(r.Context(), service.Request{
		SQL:           req.SQL,
		Approval:      approved,
		StatementHash: hash,
		MaxRows:       req.MaxRows,
		Actor:         "human:http",
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "EXEC_INVALID", "internal", err.Error(), "", false)
		return
	}

	if out.Blocked {
		if out.ApprovalRequired {
			exp := time.Now().Add(approvalTTL)
			writeJSON(w, http.StatusPreconditionRequired, errorBody{Error: errorDetail{
				Code:      "POLICY_APPROVAL_REQUIRED",
				Category:  "policy",
				Message:   out.Reason,
				Hint:      "确认风险与影响后，用返回的 token 作为 approval_token 重新提交",
				RequestID: randomToken()[:12],
				Approval: &approvalInfo{
					StatementHash: out.StatementHash,
					ExpiresAt:     exp.UTC().Format(time.RFC3339),
					Token:         s.approvalToken(out.StatementHash, exp),
				},
			}})
			return
		}
		code, status, hint := classifyBlock(out)
		writeError(w, status, code, "policy", out.Reason, hint, false)
		return
	}

	// 执行期错误必须走错误模型，不能塞进 200 响应里让客户端自己猜。
	if out.Err != nil {
		writeError(w, out.Err.HTTPStatus(), out.Err.Code, out.Err.Category,
			out.Err.Message, out.Err.Hint, out.Err.Retryable)
		return
	}

	writeJSON(w, http.StatusOK, execResponse{
		ExecutionID: out.ExecutionID,
		Risk:        out.Risk.String(),
		Friction:    out.Friction.String(),
		DurationMS:  out.Duration.Milliseconds(),
		Results:     out.Results,
	})
}

type queryRequest struct {
	SQL       string `json:"sql"`
	FetchSize int    `json:"fetch_size"`
	MaxRows   int    `json:"max_rows"`
}

type queryResponse struct {
	ExecutionID string         `json:"execution_id"`
	CursorID    string         `json:"cursor_id"`
	Columns     []model.Column `json:"columns"`
	Risk        string         `json:"risk"`
	Friction    string         `json:"friction"`
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	var req queryRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}

	out, err := e.exec.Query(r.Context(), service.Request{
		SQL:       req.SQL,
		FetchSize: req.FetchSize,
		MaxRows:   req.MaxRows,
		Actor:     "human:http",
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "EXEC_INVALID", "internal", err.Error(),
			"流式查询只接受单条只读语句；写操作请用 /exec", false)
		return
	}

	s.mu.Lock()
	s.cursorOwner[out.CursorID] = e
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, queryResponse{
		ExecutionID: out.ExecutionID,
		CursorID:    out.CursorID,
		Columns:     out.Columns,
		Risk:        out.Risk.String(),
		Friction:    out.Friction.String(),
	})
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	e := s.ownerOf(cid)
	if e == nil {
		writeError(w, http.StatusGone, "CURSOR_CLOSED", "resource", "游标已关闭或已过期", "", false)
		return
	}

	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	page, err := e.exec.Page(r.Context(), cid, limit)
	switch {
	case errors.Is(err, service.ErrCursorClosed):
		s.forgetCursor(cid)
		writeError(w, http.StatusGone, "CURSOR_CLOSED", "resource", "游标已关闭或已过期", "", false)
		return
	case errors.Is(err, service.ErrCursorBusy):
		writeError(w, http.StatusConflict, "CURSOR_BUSY", "resource", "游标正在被另一个请求读取", "", true)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "CURSOR_FAILED", "resource", err.Error(), "", false)
		return
	}

	if !page.HasMore {
		s.forgetCursor(cid)
	}
	rows := page.Rows
	if rows == nil {
		rows = [][]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":      rows,
		"has_more":  page.HasMore,
		"truncated": page.Truncated,
	})
}

func (s *Server) handleCloseCursor(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if e := s.ownerOf(cid); e != nil {
		e.exec.CloseCursor(cid)
	}
	s.forgetCursor(cid)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTableRows(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}

	q := r.URL.Query()
	limit := atoiDefault(q.Get("limit"), 100)

	var after []any
	if raw := q.Get("after"); raw != "" {
		var keys []string
		if err := json.Unmarshal([]byte(raw), &keys); err != nil {
			writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
				"after 需要是 JSON 字符串数组", `例如 after=["42"]`, false)
			return
		}
		for _, k := range keys {
			after = append(after, k)
		}
	}

	res, err := e.exec.TableRows(r.Context(), service.TableRowsRequest{
		Table: r.PathValue("name"),
		Limit: limit,
		After: after,
	})
	if errors.Is(err, service.ErrNoPrimaryKey) {
		writeError(w, http.StatusNotImplemented, "CAPABILITY_UNSUPPORTED", "internal",
			err.Error(), "无主键表只能整表浏览，无法做稳定的分页定位", false)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "TABLE_ROWS_FAILED", "internal", err.Error(), "", false)
		return
	}

	next := make([]string, len(res.Next))
	for i, v := range res.Next {
		next[i] = fmt.Sprint(v)
	}
	if res.Rows == nil {
		res.Rows = [][]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"columns":  res.Columns,
		"rows":     res.Rows,
		"next":     next,
		"has_more": res.HasMore,
	})
}

func (s *Server) ownerOf(cid string) *entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cursorOwner[cid]
}

func (s *Server) forgetCursor(cid string) {
	s.mu.Lock()
	delete(s.cursorOwner, cid)
	s.mu.Unlock()
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

type changePlanRequest struct {
	Table   model.ObjectRef     `json:"table"`
	Changes []service.RowChange `json:"changes"`
}

type changeApplyRequest struct {
	ApprovalToken string `json:"approval_token"`
}

func (s *Server) handleChangePlan(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	var req changePlanRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}

	plan, err := e.exec.BuildChangePlan(r.Context(), service.ChangeSetRequest{
		Table: req.Table, Changes: req.Changes,
	})
	if errors.Is(err, service.ErrNoPrimaryKeyForEdit) {
		writeError(w, http.StatusNotImplemented, "CAPABILITY_UNSUPPORTED", "internal",
			err.Error(), "无主键表只能通过原始 SQL 修改，无法保证定位准确", false)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "CHANGE_PLAN_FAILED", "internal", err.Error(), "", false)
		return
	}

	// 预案由服务端保管：客户端只能拿 id 回来执行，不能自造预案。
	s.mu.Lock()
	s.plans[plan.ID] = &storedPlan{plan: plan, exec: e.exec, exp: plan.ExpiresAt}
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, plan)
}

func (s *Server) handleChangeApply(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")

	s.mu.RLock()
	sp, ok := s.plans[pid]
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "CHANGE_PLAN_NOT_FOUND", "resource",
			"变更预案不存在或已被清理", "", false)
		return
	}

	var req changeApplyRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}

	approved := false
	if req.ApprovalToken != "" {
		if !s.verifyApproval(req.ApprovalToken, sp.plan.ID) {
			writeError(w, http.StatusConflict, "POLICY_APPROVAL_MISMATCH", "policy",
				"审批凭证无效、已过期，或与当前预案不匹配", "重新生成预案", false)
			return
		}
		approved = true
	}

	out, err := sp.exec.ApplyChangePlan(r.Context(), sp.plan, service.Request{
		Approval: approved, StatementHash: sp.plan.ID, Actor: "human:http",
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "CHANGE_APPLY_FAILED", "internal", err.Error(), "", false)
		return
	}

	if out.Blocked {
		if out.ApprovalRequired {
			exp := time.Now().Add(approvalTTL)
			writeJSON(w, http.StatusPreconditionRequired, errorBody{Error: errorDetail{
				Code: "POLICY_APPROVAL_REQUIRED", Category: "policy",
				Message: out.Reason,
				Hint:    "确认预案里的语句后，用返回的 token 重新提交",
				Approval: &approvalInfo{
					StatementHash: sp.plan.ID,
					ExpiresAt:     exp.UTC().Format(time.RFC3339),
					Token:         s.approvalToken(sp.plan.ID, exp),
				},
			}})
			return
		}
		code, status, hint := classifyBlock(out)
		writeError(w, status, code, "policy", out.Reason, hint, false)
		return
	}
	if out.Err != nil {
		writeError(w, out.Err.HTTPStatus(), out.Err.Code, out.Err.Category,
			out.Err.Message, out.Err.Hint, out.Err.Retryable)
		return
	}

	// 执行成功后预案作废，避免重复应用。
	s.mu.Lock()
	delete(s.plans, pid)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"execution_id": out.ExecutionID,
		"risk":         out.Risk.String(),
		"friction":     out.Friction.String(),
		"duration_ms":  out.Duration.Milliseconds(),
	})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.audit.(service.AuditReader)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rec.Recent()})
}

// classifyBlock 把策略拒绝映射为稳定的错误码与状态码。
func classifyBlock(out *service.Outcome) (code string, status int, hint string) {
	if out.Friction == policy.F5 {
		return "POLICY_READ_ONLY", http.StatusForbidden,
			"该连接为只读访问模式；如需写入请新建读写连接"
	}
	return "POLICY_APPROVAL_REQUIRED", http.StatusPreconditionRequired,
		"确认风险与影响后，用 approve=true 重新提交"
}

func (s *Server) lookup(r *http.Request) (*entry, bool) {
	id := r.PathValue("id")
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.conns[id]
	return e, ok
}

// ---- helpers ----

func decode(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, category, msg, hint string, retryable bool) {
	writeJSON(w, status, errorBody{Error: errorDetail{
		Code: code, Category: category, Message: msg, Hint: hint,
		Retryable: retryable, RequestID: randomToken()[:12],
	}})
}

func randomToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback-token"
	}
	return hex.EncodeToString(b[:])
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// IsLoopback 判断监听地址是否只对本机开放。
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Serve 启动服务并阻塞，直到 ctx 取消。
func (s *Server) Serve(ctx context.Context, addr string, allowRemote bool) error {
	if !allowRemote && !IsLoopback(addr) {
		return &AddrError{Addr: addr}
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// AddrError 表示监听地址不符合回环要求。
type AddrError struct{ Addr string }

func (e *AddrError) Error() string {
	return "拒绝监听非回环地址 " + e.Addr + "（需要 --allow-remote 显式开启）"
}
