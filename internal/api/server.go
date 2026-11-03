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
	// plans 按 id 保管变更预案。	// 只存服务端生成的预案：客户端不能回传预案，否则等于绕过风险分级。
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
		{http.MethodGet, "/api/v1/connections/{id}/tables/{name}/schema", false, s.handleTableSchema},
		{http.MethodPost, "/api/v1/connections/{id}/schema/plan", false, s.handleSchemaPlan},
		{http.MethodPost, "/api/v1/connections/{id}/bulk/plan", false, s.handleBulkPlan},
		{http.MethodPost, "/api/v1/compare", false, s.handleCompare},
		{http.MethodPost, "/api/v1/changes/{pid}/apply", false, s.handleChangeApply},
		{http.MethodPost, "/api/v1/connections/{id}/query", false, s.handleQuery},
		{http.MethodGet, "/api/v1/results/{cid}/page", false, s.handlePage},
		{http.MethodDelete, "/api/v1/results/{cid}", false, s.handleCloseCursor},
		{http.MethodGet, "/api/v1/audit", false, s.handleAudit},
		{http.MethodGet, "/api/v1/connections/{id}/export", false, s.handleExport},
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
	// Schema 与 Catalog 是两件事：PG 的 Catalog 是数据库、Schema 默认 public；
	// 把库名当 schema 传下去会让「查表」全部落空。
	Schema string `json:"schema"`
	User   string `json:"user"`
	// Password 只进不出：请求里出现一次，之后任何响应都不回显。
	Password    string `json:"password"`
	Environment string `json:"environment"`
	AccessMode  string `json:"access_mode"`
}

// connectConfigFrom 把请求体映射成连接配置。
//
// 两个容易错的地方在这里集中处理，并有单测锁住：
//   - Catalog 与 Schema 是两件事。PG 的 Catalog 是数据库、Schema 默认 public，
//     把库名当 schema 传下去会让所有「查表」落到不存在的 schema 上。
//   - SQLite 的 Host 是文件路径，Port/User/Password 对它没有意义。
func connectConfigFrom(req createConnectionRequest, env model.Environment, mode model.AccessMode) driver.ConnectConfig {
	cfg := driver.ConnectConfig{
		Name:        req.Name,
		Host:        req.Host,
		AccessMode:  mode,
		Environment: env,
	}
	if model.Engine(req.Engine) == model.EngineSQLite {
		return cfg
	}
	cfg.Port = req.Port
	cfg.Catalog = req.Catalog
	cfg.Schema = req.Schema
	cfg.User = req.User
	cfg.Password = req.Password
	return cfg
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

// undoTTL 是一键撤销的窗口，与 docs/06 §2.6 的「10 分钟内可撤销」一致。
const undoTTL = 10 * time.Minute

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

	cfg := connectConfigFrom(req, env, mode)
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

	var filter []service.FilterCond
	if raw := q.Get("where"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &filter); err != nil {
			writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
				"where 需要是 JSON 数组", `例如 where=[{"column":"status","op":"eq","value":"paid"}]`, false)
			return
		}
	}
	var order *service.OrderSpec
	if raw := q.Get("order"); raw != "" {
		var spec service.OrderSpec
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
				"order 需要是 JSON 对象", `例如 order={"column":"id","desc":true}`, false)
			return
		}
		order = &spec
	}

	res, err := e.exec.TableRows(r.Context(), service.TableRowsRequest{
		Table:  r.PathValue("name"),
		Limit:  limit,
		After:  after,
		Filter: filter,
		Order:  order,
	})
	if errors.Is(err, service.ErrInvalidFilter) {
		writeError(w, http.StatusBadRequest, "FILTER_INVALID", "internal",
			err.Error(), `where 只接受出现在表定义中的列与白名单操作符`, false)
		return
	}
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
		"columns":     res.Columns,
		"rows":        res.Rows,
		"next":        next,
		"has_more":    res.HasMore,
		"primary_key": res.PrimaryKey,
		"editable":    res.Editable,
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

// handleTableSchema 返回表结构与定义原文，供结构页渲染与 DDL 预览。
func (s *Server) handleTableSchema(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	ref := model.ObjectRef{Kind: model.KindTable, Name: r.PathValue("name"), Schema: r.URL.Query().Get("schema")}

	detail, err := e.exec.DescribeTable(r.Context(), ref)
	if errors.Is(err, driver.ErrObjectNotFound) {
		writeError(w, http.StatusNotFound, "OBJECT_NOT_FOUND", "resource",
			err.Error(), "刷新表清单后重试", false)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "SCHEMA_READ_FAILED", "internal", err.Error(), "", false)
		return
	}
	ddlText, derr := e.exec.TableDDL(r.Context(), ref)

	body := map[string]any{
		"table":        detail.Ref,
		"columns":      detail.Columns,
		"indexes":      detail.Indexes,
		"foreign_keys": detail.ForeignKeys,
		"row_estimate": detail.RowEstimate,
		"primary_key":  detail.PrimaryKeyColumns(),
		"ddl":          ddlText,
	}
	if derr != nil {
		// DDL 还原失败不该让整个结构页打不开：结构本身仍然可用。
		body["ddl"] = ""
		body["ddl_error"] = derr.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

type schemaPlanRequest struct {
	Table      model.ObjectRef      `json:"table"`
	Action     service.DDLAction    `json:"action"`
	Column     service.ColumnSpec   `json:"column"`
	Columns    []service.ColumnSpec `json:"columns"`
	PrimaryKey []string             `json:"primary_key"`
	Index      service.IndexSpec    `json:"index"`
}

// handleSchemaPlan 生成结构变更预案。执行走同一个 /changes/{pid}/apply。
func (s *Server) handleSchemaPlan(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	var req schemaPlanRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}

	plan, err := e.exec.BuildDDLPlan(r.Context(), service.DDLRequest{
		Table: req.Table, Action: req.Action, Column: req.Column,
		Columns: req.Columns, PrimaryKey: req.PrimaryKey, Index: req.Index,
	})
	if errors.Is(err, service.ErrDuplicateRows) {
		writeError(w, http.StatusConflict, "DUPLICATE_ROWS", "policy",
			err.Error(), "先清理重复值，或去掉「唯一」再建", false)
		return
	}
	if errors.Is(err, service.ErrNarrowingBlocked) {
		writeError(w, http.StatusConflict, "NARROWING_BLOCKED", "policy",
			err.Error(), "先清理越界数据，或换一个能装下的类型", false)
		return
	}
	if errors.Is(err, service.ErrInvalidDDL) {
		writeError(w, http.StatusBadRequest, "INVALID_COLUMN", "internal",
			err.Error(), "列名、类型与默认值都有白名单约束，见 docs/07 §6", false)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "SCHEMA_PLAN_FAILED", "internal", err.Error(), "", false)
		return
	}

	// preview=1 只算语句不保管预案：结构面板要边填边看 DDL，
	// 不该为每一次按键在服务端留一张可执行的预案。
	if r.URL.Query().Get("preview") == "1" {
		writeJSON(w, http.StatusOK, plan)
		return
	}

	s.mu.Lock()
	s.purgeExpiredPlansLocked()
	s.plans[plan.ID] = &storedPlan{plan: plan, exec: e.exec, exp: plan.ExpiresAt}
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, plan)
}

type bulkPlanRequest struct {
	Table            model.ObjectRef      `json:"table"`
	Set              map[string]any       `json:"set"`
	Filter           []service.FilterCond `json:"filter"`
	ConfirmTable     string               `json:"confirm_table"`
	AcceptNoRollback bool                 `json:"accept_no_rollback"`
}

// handleBulkPlan 生成条件批量修改的预案。
//
// 与数据变更集共用同一个 apply 端点：批量修改只是「按条件而不是按行」描述变更，
// 执行时的行数校验、事务回滚与审批逻辑完全一样。
func (s *Server) handleBulkPlan(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	var req bulkPlanRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}

	res, err := e.exec.BuildBulkUpdatePlan(r.Context(), service.BulkUpdateRequest{
		Table: req.Table, Set: req.Set, Filter: req.Filter,
		ConfirmTable: req.ConfirmTable, AcceptNoRollback: req.AcceptNoRollback,
	})
	switch {
	case errors.Is(err, service.ErrBulkNeedsConfirmTable):
		writeError(w, http.StatusPreconditionRequired, "BULK_CONFIRM_TABLE_REQUIRED", "policy",
			err.Error(), "把 confirm_table 设成表名再提交", false)
		return
	case errors.Is(err, service.ErrBulkNeedsNoRollbackAck):
		writeError(w, http.StatusPreconditionRequired, "BULK_NO_ROLLBACK_ACK", "policy",
			err.Error(), "确认已备份后把 accept_no_rollback 设为 true", false)
		return
	case errors.Is(err, service.ErrInvalidFilter):
		writeError(w, http.StatusBadRequest, "FILTER_INVALID", "internal", err.Error(), "", false)
		return
	case errors.Is(err, service.ErrInvalidDDL):
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "BULK_PLAN_FAILED", "internal", err.Error(), "", false)
		return
	}

	s.mu.Lock()
	s.purgeExpiredPlansLocked()
	s.plans[res.Plan.ID] = &storedPlan{plan: res.Plan, exec: e.exec, exp: res.Plan.ExpiresAt}
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, res)
}

type compareRequest struct {
	Left  string `json:"left"`
	Right string `json:"right"`
}

// handleCompare 比对两个已打开连接的结构。
//
// 只读：不生成预案、不改任何一边。返回的 upgrade/downgrade 只是文本，
// 要落地仍需走变更预案。
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	var req compareRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}
	if req.Left == "" || req.Right == "" {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
			"需要 left 与 right 两个连接 id", "", false)
		return
	}

	s.mu.RLock()
	left, lok := s.conns[req.Left]
	right, rok := s.conns[req.Right]
	s.mu.RUnlock()
	if !lok || !rok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection",
			"左侧或右侧连接不存在", "", false)
		return
	}

	res, err := service.Compare(r.Context(), left.drv, right.drv)
	if err != nil {
		writeError(w, http.StatusBadGateway, "COMPARE_FAILED", "internal", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, res)
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
	s.purgeExpiredPlansLocked()
	s.plans[plan.ID] = &storedPlan{plan: plan, exec: e.exec, exp: plan.ExpiresAt}
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, plan)
}

// purgeExpiredPlansLocked 顺手清掉过期预案。
// 没有后台清理协程，靠每次写入时扫一遍：预案数量和内存都很小，
// 比常驻 goroutine 更省事，也不会让 map 无限长。
func (s *Server) purgeExpiredPlansLocked() {
	now := time.Now()
	for id, sp := range s.plans {
		if !sp.exp.IsZero() && now.After(sp.exp) {
			delete(s.plans, id)
		}
	}
}

// buildUndoPlan 把刚执行成功预案的反向语句登记成一张可执行预案。
//
// 撤销不开后门：它就是一张普通预案，走同一套风险分级、摩擦与审批，
// 只是 ID 带 undo- 前缀且不能再被撤销。不可逆的变更（如 delete 缺整行原值、
// insert 缺自增主键回填、删列）根本没有反向语句，此时返回 nil，前端据此不显示撤销。
func (s *Server) buildUndoPlan(ctx context.Context, sp *storedPlan) *service.ChangePlan {
	if sp.plan.Undo || sp.plan.Irreversible || len(sp.plan.Rollback) == 0 {
		return nil
	}
	now := time.Now()
	undo := &service.ChangePlan{
		ID:         "undo-" + randomToken()[:16],
		Table:      sp.plan.Table,
		Statements: sp.plan.Rollback,
		Risk:       sp.plan.Risk,
		Friction:   sp.plan.Friction,
		Undo:       true,
		DDL:        sp.plan.DDL,
		// 结构变更的撤销同样要带前置条件，只是基准换成「刚执行完」的那一版定义。
		RollsBackAsCompensation: sp.plan.RollsBackAsCompensation,
		AffectedRows:            -1,
		CreatedAt:               now,
		ExpiresAt:               now.Add(undoTTL),
	}
	if sp.plan.DDL {
		if cur, err := sp.exec.TableDDL(ctx, sp.plan.Table); err == nil {
			undo.ExpectDDLHash = service.DDLHash(cur)
		}
	}
	s.mu.Lock()
	s.purgeExpiredPlansLocked()
	s.plans[undo.ID] = &storedPlan{plan: undo, exec: sp.exec, exp: undo.ExpiresAt}
	s.mu.Unlock()
	return undo
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

	body := map[string]any{
		"execution_id": out.ExecutionID,
		"risk":         out.Risk.String(),
		"friction":     out.Friction.String(),
		"duration_ms":  out.Duration.Milliseconds(),
	}
	// 撤销预案：反向语句申请时就算好了，这里只是登记成可执行预案。
	// 没有反向语句时显式给 null，让前端知道「这次改不了回来」而不是留空猜。
	if undo := s.buildUndoPlan(r.Context(), sp); undo != nil {
		body["undo"] = map[string]any{
			"id":         undo.ID,
			"expires_at": undo.ExpiresAt.UTC().Format(time.RFC3339),
			"statements": undo.Statements,
		}
	} else {
		body["undo"] = nil
	}

	writeJSON(w, http.StatusOK, body)
}

// lazyWriteWriter 把响应头与状态码推迟到「第一次真正写出数据」时才发。
// 这样导出在开跑前就失败（表不存在、格式不支持、筛选列写错）时，
// 仍然能返回一个正常的 JSON 错误，而不是一个空文件配 200。
type lazyWriteWriter struct {
	w       http.ResponseWriter
	code    int
	header  http.Header
	started bool
}

func (l *lazyWriteWriter) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		for k, vs := range l.header {
			for _, v := range vs {
				l.w.Header().Add(k, v)
			}
		}
		l.w.WriteHeader(l.code)
	}
	return l.w.Write(p)
}

func safeFilename(name, ext string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "export"
	}
	return out + "." + ext
}

// handleExport 流式导出表数据为 CSV 或 NDJSON。
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}

	q := r.URL.Query()
	table := strings.TrimSpace(q.Get("table"))
	if table == "" {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
			"缺少 table 参数", "导出以表为单位，一次一张表", false)
		return
	}
	format := service.ExportFormat(q.Get("format"))
	if format == "" {
		format = service.ExportCSV
	}
	if !format.Valid() {
		writeError(w, http.StatusBadRequest, "EXPORT_FORMAT_UNSUPPORTED", "internal",
			"不支持的导出格式: "+string(format), "可用 csv / ndjson", false)
		return
	}

	var filter []service.FilterCond
	if raw := q.Get("where"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &filter); err != nil {
			writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
				"where 需要是 JSON 数组", "", false)
			return
		}
	}
	var order *service.OrderSpec
	if raw := q.Get("order"); raw != "" {
		var spec service.OrderSpec
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
				"order 需要是 JSON 对象", "", false)
			return
		}
		order = &spec
	}

	ext := "csv"
	contentType := "text/csv; charset=utf-8"
	if format == service.ExportNDJSON {
		ext = "ndjson"
		contentType = "application/x-ndjson; charset=utf-8"
	}

	lw := &lazyWriteWriter{
		w:    w,
		code: http.StatusOK,
		header: http.Header{
			"Content-Type":        []string{contentType},
			"Content-Disposition": []string{`attachment; filename="` + safeFilename(table, ext) + `"`},
			"Cache-Control":       []string{"no-store"},
		},
	}

	res, err := e.exec.Export(r.Context(), service.ExportRequest{
		Table:    table,
		Format:   format,
		Filter:   filter,
		Order:    order,
		Header:   q.Get("header") != "0",
		PageSize: atoiDefault(q.Get("page_size"), 500),
	}, lw)
	if err != nil {
		// 已经写出过数据就只能留下截断文件；没写过才返回 JSON 错误。
		if lw.started {
			return
		}
		switch {
		case errors.Is(err, service.ErrInvalidFilter):
			writeError(w, http.StatusBadRequest, "FILTER_INVALID", "internal", err.Error(), "", false)
		case errors.Is(err, service.ErrExportFormat):
			writeError(w, http.StatusBadRequest, "EXPORT_FORMAT_UNSUPPORTED", "internal", err.Error(), "", false)
		default:
			writeError(w, http.StatusBadRequest, "EXPORT_FAILED", "internal", err.Error(), "", false)
		}
		return
	}
	// 行数只有在流写完才知道，此时响应头早已发出，所以不塞响应头。
	// 导出行数记在审计里（Export 自己会 record）。
	_ = res
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
