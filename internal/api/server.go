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
	"os"
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
//
// 返回 any 而不是 driver.Connector：关系型返回 Connector，非关系型返回
// KVConnector，由 api 用**类型断言**分流（ADR-0007）。返回值改成具体接口
// 就等于逼 api 判断引擎名，那是 ADR-0005 明令禁止的。
type Opener func(engine model.Engine, cfg driver.ConnectConfig) (any, error)

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
	// agentCfg 是模型服务配置；agentConvs 是按会话隔离的 Agent 运行时。
	agentCfg   AgentConfig
	agentConvs map[string]*agentConv
}

type storedPlan struct {
	plan *service.ChangePlan
	exec *service.Executor
	exp  time.Time
}

type entry struct {
	conn model.Connection
	// drv 与 kv 二选一：关系型走 drv，key-value 引擎走 kv。
	// 两者都不为 nil 的情况不存在——分流在 handleCreateConnection 里一次定死。
	drv  driver.Connector
	kv   driver.KVConnector
	exec *service.Executor
	// kvexec 是 key-value 的执行门（风险 → 摩擦 → 审批 → 审计），
	// 与关系型的 exec 同构但分开实现（ADR-0007）。
	kvexec *service.KVExecutor
}

// New 创建服务。token 为空时随机生成。
func New(token string, opener Opener, audit service.AuditSink) *Server {
	if token == "" {
		token = randomToken()
	}
	return &Server{token: token, opener: opener, audit: audit,
		conns: map[string]*entry{}, cursorOwner: map[string]*entry{}, plans: map[string]*storedPlan{},
		agentConvs: map[string]*agentConv{}}
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
		e.close()
	}
}

// close 释放这条连接持有的资源。drv 与 kv 二选一，所以两边都要判空。
func (e *entry) close() {
	if e.exec != nil {
		e.exec.Close()
	}
	if e.drv != nil {
		_ = e.drv.Close()
	}
	if e.kv != nil {
		_ = e.kv.Close()
	}
}

// health 取连接健康信息。关系型与 key-value 走各自的接口，调用方不用关心。
func (e *entry) health(ctx context.Context) (*driver.HealthInfo, error) {
	switch {
	case e.drv != nil:
		return e.drv.Health(ctx)
	case e.kv != nil:
		return e.kv.Health(ctx)
	default:
		return nil, errors.New("连接未初始化")
	}
}

// capabilities 返回这个连接的能力声明。
//
// 两类连接器都有 Capabilities()，合在一处取，调用方不必先判断是哪一类。
func (e *entry) capabilities() model.Capabilities {
	switch {
	case e.drv != nil:
		return e.drv.Capabilities()
	case e.kv != nil:
		return e.kv.Capabilities()
	default:
		return model.Capabilities{}
	}
}

// relational 返回关系型连接器；key-value 连接返回 false。
//
// 用它而不是在每个 handler 里判 `e.drv == nil`：把「这条连接是不是关系型」
// 收敛成一个方法，将来加第三种连接器时只要改这里。
func (e *entry) relational() (driver.Connector, bool) {
	if e.drv == nil {
		return nil, false
	}
	return e.drv, true
}

// keyValue 返回 key-value 连接器；关系型连接返回 false。
func (e *entry) keyValue() (driver.KVConnector, bool) {
	if e.kv == nil {
		return nil, false
	}
	return e.kv, true
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
		{http.MethodGet, "/api/v1/connections/{id}/search", false, s.handleSearch},
		{http.MethodGet, "/api/v1/connections/{id}/monitor/sessions", false, s.handleSessions},
		{http.MethodPost, "/api/v1/connections/{id}/monitor/sessions/{sid}/kill", false, s.handleKillSession},
		{http.MethodGet, "/api/v1/connections/{id}/tables/{name}/rows", false, s.handleTableRows},
		{http.MethodPost, "/api/v1/connections/{id}/exec", false, s.handleExec},
		{http.MethodPost, "/api/v1/connections/{id}/cancel", false, s.handleCancel},
		{http.MethodPost, "/api/v1/connections/{id}/explain", false, s.handleExplain},
		{http.MethodPost, "/api/v1/connections/{id}/changes/plan", false, s.handleChangePlan},
		{http.MethodGet, "/api/v1/connections/{id}/tables/{name}/schema", false, s.handleTableSchema},
		{http.MethodPost, "/api/v1/connections/{id}/schema/plan", false, s.handleSchemaPlan},
		{http.MethodPost, "/api/v1/connections/{id}/bulk/plan", false, s.handleBulkPlan},
		{http.MethodPost, "/api/v1/compare", false, s.handleCompare},
		{http.MethodPost, "/api/v1/migrate/plan", false, s.handleMigratePlan},
		{http.MethodPost, "/api/v1/migrate/change", false, s.handleMigrateChange},
		{http.MethodPost, "/api/v1/migrate/run", false, s.handleMigrateRun},
		{http.MethodPost, "/api/v1/connections/{id}/fk/precheck", false, s.handleFKPrecheck},
		{http.MethodPost, "/api/v1/changes/{pid}/apply", false, s.handleChangeApply},
		{http.MethodPost, "/api/v1/connections/{id}/query", false, s.handleQuery},
		{http.MethodGet, "/api/v1/results/{cid}/page", false, s.handlePage},
		{http.MethodDelete, "/api/v1/results/{cid}", false, s.handleCloseCursor},
		{http.MethodGet, "/api/v1/audit", false, s.handleAudit},
		{http.MethodGet, "/api/v1/audit/verify", false, s.handleAuditVerify},
		{http.MethodGet, "/api/v1/connections/{id}/export", false, s.handleExport},
		// key-value 端点用查询参数传 key，不放进路径：Redis 的 key 是二进制安全的，
		// 冒号、斜杠、空格都合法，放进路径会被 URL 转义规则咬住。
		{http.MethodGet, "/api/v1/connections/{id}/kv/keys", false, s.handleKVKeys},
		{http.MethodGet, "/api/v1/connections/{id}/kv/entry", false, s.handleKVEntry},
		{http.MethodGet, "/api/v1/connections/{id}/kv/value", false, s.handleKVValue},
		{http.MethodPost, "/api/v1/connections/{id}/kv/command", false, s.handleKVCommand},
		{http.MethodPost, "/api/v1/connections/{id}/kv/apply", false, s.handleKVApply},
		// Agent：会话绑定连接与模式，预案在会话内保管，批准入口只给人用。
		{http.MethodGet, "/api/v1/agent/status", false, s.handleAgentStatus},
		{http.MethodGet, "/api/v1/agent/tools", false, s.handleAgentTools},
		{http.MethodPost, "/api/v1/agent/conversations", false, s.handleAgentConversationCreate},
		{http.MethodGet, "/api/v1/agent/conversations/{id}", false, s.handleAgentConversation},
		{http.MethodPost, "/api/v1/agent/conversations/{id}/abort", false, s.handleAgentAbort},
		{http.MethodPost, "/api/v1/agent/conversations/{id}/compact", false, s.handleAgentCompact},
		{http.MethodPost, "/api/v1/agent/conversations/{id}/messages", false, s.handleAgentMessage},
		{http.MethodPost, "/api/v1/agent/conversations/{id}/approvals/{pid}", false, s.handleAgentApproval},
		{http.MethodGet, "/api/v1/agent/conversations/{id}/export", false, s.handleAgentExport},
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
	// DisplayTimeZone 是时间值的展示时区（IANA 名）。空 = UTC。
	// 只影响展示：会话时区固定 UTC，写进去的就是 UTC。
	DisplayTimeZone string `json:"display_timezone"`
}

// connectConfigFrom 把请求体映射成连接配置。
//
// 两个容易错的地方在这里集中处理，并有单测锁住：
//   - Catalog 与 Schema 是两件事。PG 的 Catalog 是数据库、Schema 默认 public，
//     把库名当 schema 传下去会让所有「查表」落到不存在的 schema 上。
//   - SQLite 的 Host 是文件路径，Port/User/Password 对它没有意义。
func connectConfigFrom(req createConnectionRequest, env model.Environment, mode model.AccessMode) driver.ConnectConfig {
	cfg := driver.ConnectConfig{
		Name:            req.Name,
		Host:            req.Host,
		AccessMode:      mode,
		Environment:     env,
		DisplayTimeZone: req.DisplayTimeZone,
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
	// Kind 是「这条连接该用哪一族端点」：relational | keyvalue。
	// 由类型断言得出，不让界面去记「哪些引擎是 NoSQL」（ADR-0007）。
	Kind            string `json:"kind"`
	DisplayTimeZone string `json:"display_timezone,omitempty"`
}

type execRequest struct {
	SQL string `json:"sql"`
	// ApprovalToken 是服务端签发的审批凭证，绑定语句指纹与有效期。
	// 客户端不能用布尔值绕过 —— 审批的是「这段 SQL」，不是一个请求。
	ApprovalToken string `json:"approval_token"`
	MaxRows       int    `json:"max_rows"`
	// Cursor 是编辑器里光标的字节偏移；给了就只执行光标所在的那条语句。
	// 切分在服务端做（前端不重复实现方言规则）。
	Cursor *int `json:"cursor"`
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
	// Results 是「多语句脚本里，失败之前已经跑完的那几条」的结果。
	//
	// 没有它的话，`SELECT 1; SELECT * FROM 不存在; SELECT 3` 只会返回一个错误，
	// 第一条明明已经成功了、数据也取回来了，用户却看不到（docs/05 验收 4）。
	// 错误仍走错误模型（非 2xx + 结构化 code），只是把已完成的部分一并带回来。
	Results []service.StatementResult `json:"results,omitempty"`
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
	// 时区名打错不能静默退回 UTC：那会让人对着差 8 小时的数据排查半天。
	if tz := strings.TrimSpace(req.DisplayTimeZone); tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
				"display_timezone 不是合法的 IANA 时区名: "+tz,
				"例如 Asia/Shanghai、UTC；留空表示按 UTC 展示", false)
			return
		}
	}

	cfg := connectConfigFrom(req, env, mode)
	engine := model.Engine(req.Engine)

	opened, err := s.opener(engine, cfg)
	if err != nil {
		// 分类由驱动给：同一句「连接失败」对不同成因毫无区分度（FR-CON-03）。
		code, hint := connectFailure(err)
		writeError(w, http.StatusBadGateway, code, "connection", err.Error(), hint, false)
		return
	}

	s.mu.Lock()
	s.seq++
	id := "conn-" + itoa(s.seq)
	e := &entry{
		conn: model.Connection{
			ID: id, Name: req.Name, Engine: engine, Host: req.Host,
			Environment: env, AccessMode: mode,
			DisplayTimeZone: strings.TrimSpace(req.DisplayTimeZone),
		},
	}
	// 用类型断言分流，不看引擎名（ADR-0007）。关系型需要执行门，
	// key-value 走自己的 KV 端点，不构造 Executor。
	switch v := opened.(type) {
	case driver.Connector:
		e.drv = v
		e.exec = service.NewExecutor(v, env, mode, s.audit)
	case driver.KVConnector:
		e.kv = v
		e.kvexec = service.NewKVExecutor(v, env, mode, s.audit)
	default:
		s.mu.Unlock()
		writeError(w, http.StatusBadGateway, "DB_CONNECT_FAILED", "connection",
			"驱动返回了无法识别的连接器类型", "", false)
		return
	}
	s.conns[id] = e
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, connectionDTO{
		ID: id, Name: req.Name, Engine: string(engine),
		Environment: string(env), AccessMode: string(mode),
		Kind:            kindOf(e),
		DisplayTimeZone: strings.TrimSpace(req.DisplayTimeZone),
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
			Kind:            kindOf(e),
			DisplayTimeZone: e.conn.DisplayTimeZone,
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
	e.close()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConnHealth(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	h, err := e.health(r.Context())
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
		// 能力声明必须能在界面上拿到，否则「引擎不支持就别摆这个入口」只能靠
		// 点一次再报错（ADR-0005）。
		"capabilities": e.capabilities(),
	})
}

// searchKinds 是 kind 过滤允许的对象类型。写成清单而不是自由字符串：拼错一个
// 类型名如果被静默忽略，用户会以为「这个库里真的没有视图」。
var searchKinds = map[model.ObjectKind]bool{
	model.KindTable:    true,
	model.KindView:     true,
	model.KindMatView:  true,
	model.KindSequence: true,
	model.KindTrigger:  true,
	model.KindType:     true,
	model.KindRoutine:  true,
}

// monitorProvider 取监控能力；不支持时把「为什么」写清楚（能力位是唯一判据）。
func (s *Server) monitorProvider(w http.ResponseWriter, r *http.Request) (driver.MonitorProvider, bool) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return nil, false
	}
	drv, isRel := e.relational()
	if !isRel {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个 key-value 连接，没有会话概念", "", false)
		return nil, false
	}
	if !drv.Capabilities().SessionMgmt {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			fmt.Sprintf("%s 没有会话列表（当前只做了 PostgreSQL 与 MySQL）", drv.Engine()), "", false)
		return nil, false
	}
	mp, ok := drv.(driver.MonitorProvider)
	if !ok {
		// 能力位说支持、类型又断言不到：这是实现缺陷，不是用户的问题。
		writeError(w, http.StatusInternalServerError, "CAPABILITY_MISMATCH", "internal",
			"能力位声明了会话管理，但驱动没有实现 MonitorProvider", "", false)
		return nil, false
	}
	return mp, true
}

// handleSessions 列出会话（FR-MON-02）。
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	mp, ok := s.monitorProvider(w, r)
	if !ok {
		return
	}
	items, err := mp.Sessions(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "DB_METADATA_FAILED", "connection", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleKillSession 结束一条会话。
//
// 这是**写操作且不可逆**（对方正在跑的语句会被掐断），所以要求连接可写；
// 只读连接上不摆这个入口，也不要让它偷偷生效。
func (s *Server) handleKillSession(w http.ResponseWriter, r *http.Request) {
	mp, ok := s.monitorProvider(w, r)
	if !ok {
		return
	}
	e, _ := s.lookup(r)
	if !e.conn.AccessMode.AllowsWrite() {
		writeError(w, http.StatusForbidden, "READ_ONLY_CONNECTION", "policy",
			"只读连接不能终止会话", "改用可写连接，或在数据库侧处理", false)
		return
	}
	if !e.drv.Capabilities().KillSession {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这个引擎不支持终止会话", "", false)
		return
	}
	sid := r.PathValue("sid")
	if !driver.ValidSessionID(sid) {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
			"会话 id 必须是数字", "", false)
		return
	}
	if err := mp.KillSession(r.Context(), sid); err != nil {
		writeError(w, http.StatusBadGateway, "DB_EXEC_FAILED", "connection", err.Error(), "", false)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"killed": sid})
}

// connectFailure 把驱动的连接失败类别映射成错误码与一句能照着做的建议（FR-CON-03）。
//
// 为什么不让用户看驱动原文：同一句连接被拒，对懂网络的人是一句话，对大多数人等于
// 没说。类别由驱动给（它认识引擎错误码），这里只负责翻成协议里的错误码。
func connectFailure(err error) (code, hint string) {
	var ce *driver.ConnectError
	if !errors.As(err, &ce) {
		return "DB_CONNECT_FAILED", ""
	}
	switch ce.Kind {
	case driver.ConnectDNS:
		return "DB_CONNECT_DNS", "域名解析不到：检查主机名拼写，以及当前网络 / VPN 能不能解析它"
	case driver.ConnectRefused:
		return "DB_CONNECT_REFUSED", "端口拒绝连接：确认服务在跑、端口对、防火墙或安全组已放通"
	case driver.ConnectTimeout:
		return "DB_CONNECT_TIMEOUT", "连接超时：网络不通或被中间设备丢弃；先试着 telnet 该主机的这个端口"
	case driver.ConnectAuth:
		return "DB_CONNECT_AUTH", "认证失败：用户名或口令不对；也可能是这个用户不允许从当前机器登录"
	case driver.ConnectNoDatabase:
		return "DB_CONNECT_NO_DATABASE", "库不存在：确认库名（PG 与 MySQL 连的都是 database，不是表名）"
	case driver.ConnectTLS:
		return "DB_CONNECT_TLS", "TLS 握手失败：证书、CA 或加密模式与实例要求不一致"
	case driver.ConnectHandshake:
		return "DB_CONNECT_HANDSHAKE", "TCP 通了但握手被切断：对端可能不是这个引擎，也可能被代理 / 负载均衡掐掉了；确认端口指向的服务与版本"
	case driver.ConnectFile:
		return "DB_CONNECT_FILE", "文件打不开：检查路径、所在目录的权限，以及连接是不是只读模式"
	}
	return "DB_CONNECT_FAILED", ""
}

// handleSearch 按名称/注释模糊搜索对象（FR-META-04）。
//
// 驱动层的 `Meta().Search` 早就实现了（三引擎都有，PG/MySQL 还搜注释），但一直
// 没有 HTTP 出口——需求上这条是 P0，实际等于没做。
//
// kind 过滤放在这一层：驱动只认「关键字 + 条数」，不该知道上层想过滤哪些类型。
// 代价是可能少于 limit（驱动先截断再过滤），所以响应里给 truncated 让人知道。
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	drv, isRel := e.relational()
	if !isRel {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个 key-value 连接，没有对象清单可搜",
			"改用 GET /api/v1/connections/{id}/kv/keys?pattern=...", false)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
			"q 必填（按名称或注释模糊匹配）", "", false)
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
				"limit 必须是正整数", "", false)
			return
		}
		if n > 200 {
			n = 200
		}
		limit = n
	}
	kinds := map[model.ObjectKind]bool{}
	if raw := strings.TrimSpace(r.URL.Query().Get("kind")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			k := model.ObjectKind(strings.TrimSpace(part))
			if k == "" {
				continue
			}
			if !searchKinds[k] {
				writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
					"kind 只接受 table / view / matview / sequence / trigger / type / routine", "", false)
				return
			}
			kinds[k] = true
		}
	}
	objs, err := drv.Meta().Search(r.Context(), q, limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, "DB_METADATA_FAILED", "connection", err.Error(), "", true)
		return
	}
	truncated := len(objs) == limit
	if len(kinds) > 0 {
		kept := objs[:0]
		for _, o := range objs {
			if kinds[o.Kind] {
				kept = append(kept, o)
			}
		}
		objs = kept
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": objs, "query": q, "truncated": truncated})
}
func (s *Server) handleTables(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	drv, ok := e.relational()
	if !ok {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个 key-value 连接，没有表的概念", "改用 GET /api/v1/connections/{id}/kv/keys", false)
		return
	}
	// 对象树列什么，由能力声明决定：说了支持触发器/序列却不出现在树上，
	// 等于把「查得到」这件事藏起来（docs/04 §3.2）。
	caps := drv.Capabilities()
	kinds := []model.ObjectKind{model.KindTable, model.KindView}
	if caps.Triggers {
		kinds = append(kinds, model.KindTrigger)
	}
	if caps.Sequences {
		kinds = append(kinds, model.KindSequence)
	}
	// 用户类型（枚举 / 域）也是对象：声明了却不出现在树上，等于把「查得到」藏起来。
	if caps.UserTypes {
		kinds = append(kinds, model.KindType)
	}
	// 例程（存储过程 / 函数）同理。
	if caps.Routines {
		kinds = append(kinds, model.KindRoutine)
	}
	objs, err := drv.Meta().Objects(r.Context(), "", "", kinds...)
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
		Cursor:        req.Cursor,
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
		writeErrorWithResults(w, out.Err.HTTPStatus(), out.Err.Code, out.Err.Category,
			out.Err.Message, out.Err.Hint, out.Err.Retryable, out.Results)
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
	drv, isRel := e.relational()
	if !isRel {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个 key-value 连接，没有表结构", "改用 GET /api/v1/connections/{id}/kv/entry?key=...", false)
		return
	}
	// kind=trigger 时只回定义：触发器没有列结构，套用 DescribeTable 只会
	// 得到一个「对象不存在」。
	kind := model.KindTable
	switch r.URL.Query().Get("kind") {
	case "", "table", "view":
		kind = model.KindTable
	case "trigger":
		kind = model.KindTrigger
	case "sequence":
		kind = model.KindSequence
	case "type":
		kind = model.KindType
	case "routine":
		kind = model.KindRoutine
	default:
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
			"kind 只支持 table / view / trigger / sequence / type / routine", "", false)
		return
	}
	ref := model.ObjectRef{Kind: kind, Name: r.PathValue("name"), Schema: r.URL.Query().Get("schema")}

	if kind == model.KindTrigger || kind == model.KindSequence || kind == model.KindType ||
		kind == model.KindRoutine {
		ddlText, derr := e.exec.TableDDL(r.Context(), ref)
		if errors.Is(derr, driver.ErrObjectNotFound) {
			writeError(w, http.StatusNotFound, "OBJECT_NOT_FOUND", "resource",
				derr.Error(), "刷新对象清单后重试", false)
			return
		}
		if derr != nil {
			writeError(w, http.StatusBadRequest, "SCHEMA_READ_FAILED", "internal", derr.Error(), "", false)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object":       ref,
			"ddl":          ddlText,
			"capabilities": drv.Capabilities(),
		})
		return
	}

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
		// 这个响应是自建 DTO：TableDetail 新增字段不会自动带过来，漏一次就是
		// 「后端有、界面没有」，而且不会报任何错。下面几项都属于 docs/01
		// FR-META-03 的「对象详情」——checks 之前就一直漏着，结构页从来没列过
		// 已有的检查约束。
		"checks":       detail.Checks,
		"partitions":   detail.Partitions,
		"comment":      detail.Comment,
		"size_bytes":   detail.SizeBytes,
		"row_estimate": detail.RowEstimate,
		"primary_key":  detail.PrimaryKeyColumns(),
		"ddl":          ddlText,
		// 结构页据能力决定摆不摆某个入口：不可用的能力不出现，
		// 而不是出现了再让人点出一个必然失败的错。
		"capabilities": drv.Capabilities(),
	}
	if derr != nil {
		// DDL 还原失败不该让整个结构页打不开：结构本身仍然可用。
		body["ddl"] = ""
		body["ddl_error"] = derr.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

type schemaPlanRequest struct {
	Table      model.ObjectRef        `json:"table"`
	Action     service.DDLAction      `json:"action"`
	Column     service.ColumnSpec     `json:"column"`
	Columns    []service.ColumnSpec   `json:"columns"`
	PrimaryKey []string               `json:"primary_key"`
	Index      service.IndexSpec      `json:"index"`
	ForeignKey service.ForeignKeySpec `json:"foreign_key"`
	// Check 与 View 是 add_check / create_view 的入参。
	//
	// 漏了它们不会报编译错：decode 用 DisallowUnknownFields，请求会以
	// 「json: unknown field」被拒——服务层测试全绿、界面点了才炸。
	Check    service.CheckSpec    `json:"check"`
	View     service.ViewSpec     `json:"view"`
	Sequence service.SequenceSpec `json:"sequence"`
	Trigger  service.TriggerSpec  `json:"trigger"`
	Type     service.TypeSpec     `json:"type"`
	Routine  service.RoutineSpec  `json:"routine"`
	RawSQL   string               `json:"sql"`
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
		ForeignKey: req.ForeignKey, Check: req.Check, View: req.View,
		Sequence: req.Sequence, Trigger: req.Trigger, Type: req.Type,
		Routine: req.Routine, RawSQL: req.RawSQL,
	})
	if errors.Is(err, service.ErrLockWait) {
		writeError(w, http.StatusConflict, "LOCK_WAIT_BLOCKED", "connection",
			err.Error(), "先处理等锁的会话（结束长事务或 kill 掉），再重新生成预案", false)
		return
	}
	if errors.Is(err, service.ErrDependencyBlocked) {
		writeError(w, http.StatusConflict, "DEPENDENCY_BLOCKED", "policy",
			err.Error(), "先删掉引用它的视图或外键，再删这个对象", false)
		return
	}
	if errors.Is(err, service.ErrForeignKeyBlocked) {
		writeError(w, http.StatusConflict, "FK_PRECHECK_BLOCKED", "policy",
			err.Error(), "先清理孤儿数据，或改成能对上父表的引用列", false)
		return
	}
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
	case errors.Is(err, service.ErrLargeNumberAsJSON):
		writeError(w, http.StatusBadRequest, "NUMBER_PRECISION_LOSS", "internal", err.Error(),
			"把该列的值改成字符串形式（例如 \"9223372036854775807\"）再提交", false)
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

type fkPrecheckRequest struct {
	Child      model.ObjectRef `json:"child"`
	Parent     model.ObjectRef `json:"parent"`
	Columns    []string        `json:"columns"`
	RefColumns []string        `json:"ref_columns"`
}

// handleFKPrecheck 检查「现在加这个外键会不会踩到既有数据」。
//
// 只读。加外键的 DDL 生成暂未实现：SQLite 不支持 ALTER TABLE ADD CONSTRAINT，
// 而 MySQL/PG 目前没有可用的真机环境验收，先把不会出错的那半做出来。
func (s *Server) handleFKPrecheck(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	var req fkPrecheckRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}

	res, err := e.exec.PrecheckForeignKey(r.Context(), service.FKPrecheckRequest{
		Child: req.Child, Parent: req.Parent,
		Columns: req.Columns, RefColumns: req.RefColumns,
	})
	if errors.Is(err, service.ErrInvalidDDL) {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "FK_PRECHECK_FAILED", "internal", err.Error(), "", true)
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
	if errors.Is(err, service.ErrLargeNumberAsJSON) {
		writeError(w, http.StatusBadRequest, "NUMBER_PRECISION_LOSS", "internal", err.Error(),
			"把该列的值改成字符串形式（例如 \"9223372036854775807\"）再提交", false)
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

// applyWriter 把「普通 JSON 响应」与「NDJSON 进度流」统一起来。
//
// 关键约束：状态码只在还没发出第一条进度时才能改。一旦进度开始，
// 响应头就已经定了，后续只能用一条 {"type":"error"} 行把问题讲清楚——
// 不能在流里再插一个 HTTP 428，那时候前端已经在读 200 的 body 了。
type applyWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	stream  bool
	started bool
}

func (a *applyWriter) progress(done, total int64) {
	if a == nil || !a.stream {
		return
	}
	if !a.started {
		a.started = true
		h := a.w.Header()
		h.Set("Content-Type", "application/x-ndjson; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		// 反向代理不能把流缓冲起来，否则进度会一次性到达，等于没有进度。
		h.Set("X-Accel-Buffering", "no")
		a.w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(a.w).Encode(map[string]any{"type": "progress", "done": done, "total": total})
	a.flush()
}

func (a *applyWriter) flush() {
	if a.flusher != nil {
		a.flusher.Flush()
	}
}

// send 返回最终结果。已进入流模式时改成一条 result 行，状态码放在行里。
func (a *applyWriter) send(status int, v any) {
	if a.stream && a.started {
		_ = json.NewEncoder(a.w).Encode(map[string]any{"type": "result", "status": status, "body": v})
		a.flush()
		return
	}
	writeJSON(a.w, status, v)
}

// fail 返回错误。已进入流模式时降级为一条 error 行。
func (a *applyWriter) fail(status int, body errorBody) {
	if a.stream && a.started {
		_ = json.NewEncoder(a.w).Encode(map[string]any{"type": "error", "status": status, "error": body.Error})
		a.flush()
		return
	}
	writeJSON(a.w, status, body)
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

	// ?stream=1 时用 NDJSON 逐批回报进度。只有分批批量修改会真的产生进度事件；
	// 其余预案仍然得到一份普通 JSON 响应，前端按 Content-Type 分辨。
	aw := &applyWriter{w: w, stream: r.URL.Query().Get("stream") == "1"}
	if f, ok := w.(http.Flusher); ok {
		aw.flusher = f
	}

	out, err := sp.exec.ApplyChangePlanWithProgress(r.Context(), sp.plan, service.Request{
		Approval: approved, StatementHash: sp.plan.ID, Actor: "human:http",
	}, aw.progress)
	if err != nil {
		aw.fail(http.StatusBadRequest, errorBody{Error: errorDetail{
			Code: "CHANGE_APPLY_FAILED", Category: "internal", Message: err.Error(),
		}})
		return
	}

	if out.Blocked {
		if out.ApprovalRequired {
			exp := time.Now().Add(approvalTTL)
			aw.fail(http.StatusPreconditionRequired, errorBody{Error: errorDetail{
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
		aw.fail(status, errorBody{Error: errorDetail{
			Code: code, Category: "policy", Message: out.Reason, Hint: hint,
		}})
		return
	}
	if out.Err != nil {
		aw.fail(out.Err.HTTPStatus(), errorBody{Error: errorDetail{
			Code: out.Err.Code, Category: out.Err.Category,
			Message: out.Err.Message, Hint: out.Err.Hint, Retryable: out.Err.Retryable,
		}})
		return
	}

	// 中止：已经提交的批留在库里。预案刻意保留，用户可以重新提交把剩下的行改完
	// （SET 是常量，重复执行幂等），所以这里不能当成功把预案删掉。
	if out.Aborted {
		aw.fail(http.StatusConflict, errorBody{Error: errorDetail{
			Code: "CHANGE_APPLY_ABORTED", Category: "policy",
			Message: out.Reason,
			Hint: fmt.Sprintf("已提交 %d 行且不回滚；预案仍有效，可重新提交改完剩余行",
				out.RowsAffected),
		}})
		return
	}

	// 执行成功后预案作废，避免重复应用。
	s.mu.Lock()
	delete(s.plans, pid)
	s.mu.Unlock()

	body := map[string]any{
		"execution_id":  out.ExecutionID,
		"risk":          out.Risk.String(),
		"friction":      out.Friction.String(),
		"duration_ms":   out.Duration.Milliseconds(),
		"rows_affected": out.RowsAffected,
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

	aw.send(http.StatusOK, body)
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

	// 扩展名与 Content-Type 跟着格式走。只认 ndjson 一种的话，导出的 SQL 脚本
	// 会叫 table.csv、xlsx 会报 text/csv——浏览器与 shell 都会按错的类型处理。
	ext, contentType := "csv", "text/csv; charset=utf-8"
	switch format {
	case service.ExportNDJSON:
		ext, contentType = "ndjson", "application/x-ndjson; charset=utf-8"
	case service.ExportSQL:
		ext, contentType = "sql", "application/sql; charset=utf-8"
	case service.ExportMarkdown:
		ext, contentType = "md", "text/markdown; charset=utf-8"
	case service.ExportXLSX:
		ext = "xlsx"
		contentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
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
	// 具体预检给了错误码就用它：这类拦截不是「策略要求确认」，而是这条语句
	// 在这个引擎上没法执行，用 428 会把客户端引到错误的重试路径上。
	if out.BlockCode != "" {
		return out.BlockCode, http.StatusBadRequest, out.BlockHint
	}
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

// writeErrorWithResults 与 writeError 相同，但把「失败之前已完成的部分」一并带回去。
//
// 状态码仍是错误码：错误语义不变，只是不再把已完成的语句结果丢掉。
func writeErrorWithResults(w http.ResponseWriter, status int, code, category, msg, hint string,
	retryable bool, results []service.StatementResult) {
	writeJSON(w, status, errorBody{Error: errorDetail{
		Code: code, Category: category, Message: msg, Hint: hint,
		Retryable: retryable, RequestID: randomToken()[:12],
		Results: results,
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
	if allowRemote {
		// 说清楚风险：TLS 还没实现，远程模式就是明文 HTTP。文档里也这么写，
		// 但没人会把文档贴在屏幕上——启动时喊一句更有效。
		fmt.Fprintf(os.Stderr, "警告：-allow-remote 是明文 HTTP（TLS 未实现），只应在受信网络或反向代理之后使用\n")
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

// ---- key-value 端点 ----
//
// 这一族端点的路径里没有引擎名：Mongo / ES 将来也走同一族，只是内部映射不同
// （ADR-0007）。对关系型连接调用它们会得到 400 CAPABILITY_UNSUPPORTED。

// kvConn 取 key-value 连接器，顺便把「这不是 kv 连接」的错误写出去。
func (s *Server) kvConn(w http.ResponseWriter, r *http.Request) (driver.KVConnector, bool) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return nil, false
	}
	kv, ok := e.keyValue()
	if !ok {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个关系型连接，没有 key 的概念", "改用 /tables 与 /tables/{name}/rows", false)
		return nil, false
	}
	return kv, true
}

func (s *Server) handleKVKeys(w http.ResponseWriter, r *http.Request) {
	kv, ok := s.kvConn(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	count, _ := strconv.Atoi(q.Get("count"))
	page, err := kv.Scan(r.Context(), driver.KVScanOptions{
		Cursor:  q.Get("cursor"),
		Pattern: q.Get("pattern"),
		Type:    driver.KVType(q.Get("type")),
		Count:   count,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "KV_SCAN_FAILED", "internal", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleKVEntry(w http.ResponseWriter, r *http.Request) {
	kv, ok := s.kvConn(w, r)
	if !ok {
		return
	}
	key := r.URL.Query().Get("key")
	if strings.TrimSpace(key) == "" {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", "缺少 key", "", false)
		return
	}
	entry, err := kv.Inspect(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusBadGateway, "KV_INSPECT_FAILED", "internal", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleKVValue(w http.ResponseWriter, r *http.Request) {
	kv, ok := s.kvConn(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	key := q.Get("key")
	if strings.TrimSpace(key) == "" {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", "缺少 key", "", false)
		return
	}
	count, _ := strconv.Atoi(q.Get("count"))
	maxBytes, _ := strconv.Atoi(q.Get("max_bytes"))
	val, err := kv.Value(r.Context(), key, driver.KVValueOptions{
		Cursor: q.Get("cursor"), Count: count, MaxBytes: maxBytes,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "KV_VALUE_FAILED", "internal", err.Error(), "", true)
		return
	}
	writeJSON(w, http.StatusOK, val)
}

// kvApplyRequest 是 /kv/apply 的请求体：一次结构化的写操作。
type kvApplyRequest struct {
	Mutation      driver.KVMutation `json:"mutation"`
	ApprovalToken string            `json:"approval_token"`
}

// handleKVCommand 是命令模式的入口。
//
// 走与 SQL 同一套执行门（service.KVExecutor）：分级 → 摩擦 → 审批 → 审计。
// 被策略封禁的命令（KEYS / DEBUG）直接 403；需要确认的命令返回 428 并带上
// 绑定**命令指纹**的凭证——拿 A 的凭证执行 B 会被拒。
func (s *Server) handleKVCommand(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	if e.kvexec == nil {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个关系型连接，没有命令模式", "改用 POST /connections/{id}/exec", false)
		return
	}
	var req struct {
		Name          string `json:"name"`
		Args          []any  `json:"args"`
		ApprovalToken string `json:"approval_token"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}

	fp := service.CommandFingerprint(req.Name, req.Args)
	approved := false
	if req.ApprovalToken != "" {
		if !s.verifyApproval(req.ApprovalToken, fp) {
			writeError(w, http.StatusConflict, "POLICY_APPROVAL_MISMATCH", "policy",
				"审批凭证无效、已过期，或与当前命令不匹配", "重新执行一次取得新凭证", false)
			return
		}
		approved = true
	}

	out := e.kvexec.ExecuteCommand(r.Context(), service.KVCommandRequest{
		Name: req.Name, Args: req.Args, Approval: approved, StatementHash: fp,
	}, "human:http")
	s.writeKVOutcome(w, out, fp)
}

// handleKVApply 是结构化写操作的入口。
func (s *Server) handleKVApply(w http.ResponseWriter, r *http.Request) {
	e, ok := s.lookup(r)
	if !ok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection", "连接不存在", "", false)
		return
	}
	if e.kvexec == nil {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"这是一个关系型连接，没有 key-value 写操作", "改用 /changes/plan + /changes/{pid}/apply", false)
		return
	}
	var req kvApplyRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}

	fp := service.MutationFingerprint(req.Mutation)
	approved := false
	if req.ApprovalToken != "" {
		if !s.verifyApproval(req.ApprovalToken, fp) {
			writeError(w, http.StatusConflict, "POLICY_APPROVAL_MISMATCH", "policy",
				"审批凭证无效、已过期，或与当前操作不匹配", "重新提交取得新凭证", false)
			return
		}
		approved = true
	}

	out := e.kvexec.ApplyMutation(r.Context(), service.KVMutationRequest{
		Op: req.Mutation, Approval: approved, StatementHash: fp,
	}, "human:http")
	s.writeKVOutcome(w, out, fp)
}

// writeKVOutcome 把执行门的结果映射成 HTTP。与 SQL 路径的映射保持同构：
// 被审批拦下 → 428 并签发凭证；被策略阻断 → 403；执行出错 → 502。
func (s *Server) writeKVOutcome(w http.ResponseWriter, out *service.KVOutcome, fingerprint string) {
	switch {
	case out.ApprovalRequired:
		exp := time.Now().Add(approvalTTL)
		writeJSON(w, http.StatusPreconditionRequired, errorBody{Error: errorDetail{
			Code: "POLICY_APPROVAL_REQUIRED", Category: "policy",
			Message: out.Reason,
			Hint:    "确认命令与风险后，用返回的 token 作为 approval_token 重新提交",
			Approval: &approvalInfo{
				StatementHash: fingerprint,
				ExpiresAt:     exp.UTC().Format(time.RFC3339),
				Token:         s.approvalToken(fingerprint, exp),
			},
		}})
	case out.Blocked:
		code, hint := "POLICY_COMMAND_BLOCKED", "用 keyspace 浏览器或命令里的替代写法"
		if out.ReadOnly {
			code, hint = "POLICY_READ_ONLY", "换一个只读命令，或新建读写连接"
		}
		writeError(w, http.StatusForbidden, code, "policy", out.Reason, hint, false)
	case out.Err != nil:
		writeError(w, http.StatusBadGateway, out.Err.Code, out.Err.Category,
			out.Err.Message, out.Err.Hint, out.Err.Retryable)
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"risk":     out.Risk.String(),
			"friction": out.Friction.String(),
			"result":   out.Result,
		})
	}
}

// kindOf 说明这条连接该用哪一族端点。
//
// 由类型断言得出而不是查引擎名表：将来加 Mongo/ES 时，只要它们实现
// KVConnector，界面就自动按 key-value 渲染（ADR-0007）。
func kindOf(e *entry) string {
	if e.kv != nil {
		return "keyvalue"
	}
	return "relational"
}

// migratePlanRequest 是迁移预检的请求体。
type migratePlanRequest struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Table  string `json:"table"`
	// TargetTable 省略时与 Table 相同。
	TargetTable string `json:"target_table"`
	// Decisions 是「人工决定的目标类型」：列名 → 类型。
	Decisions map[string]string `json:"decisions"`
	// Probe 为 true 时读源表实际最大字符长度，用它判定「声明收窄」是否真会丢数据。
	// 代价是一次全表扫描，所以不做默认行为。
	Probe bool `json:"probe"`
}

// handleMigratePlan 给出迁移预检与目标端 DDL（只读，不执行）。
//
// 只读：不建表、不搬数据。ready=false 表示还有列需要人工决定类型，此时 plan
// 仍然返回，界面据此逐列提问。
func (s *Server) handleMigratePlan(w http.ResponseWriter, r *http.Request) {
	var req migratePlanRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}
	if req.Source == "" || req.Target == "" || req.Table == "" {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
			"需要 source、target、table", "", false)
		return
	}

	s.mu.RLock()
	src, sok := s.conns[req.Source]
	dst, tok := s.conns[req.Target]
	s.mu.RUnlock()
	if !sok || !tok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection",
			"源或目标连接不存在", "", false)
		return
	}
	if src.exec == nil || dst.exec == nil {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"迁移预检只支持关系型连接", "", false)
		return
	}

	plan, ddlText, err := service.PlanMigrationBetween(r.Context(), src.exec, dst.exec, req.Table, req.TargetTable, req.Decisions)
	if err != nil && !errors.Is(err, service.ErrMigrateNeedsDecision) {
		writeError(w, http.StatusBadGateway, "MIGRATE_PLAN_FAILED", "internal", err.Error(), "", true)
		return
	}
	if req.Probe && plan != nil {
		probedDDL, recomputed, probeErr := service.ProbeMigrationLengths(r.Context(), src.exec, dst.exec,
			req.Table, req.TargetTable, req.Decisions, plan)
		if probeErr != nil && !errors.Is(probeErr, service.ErrMigrateNeedsDecision) {
			writeError(w, http.StatusBadGateway, "MIGRATE_PROBE_FAILED", "internal", probeErr.Error(), "", true)
			return
		}
		if recomputed {
			ddlText, err = probedDDL, probeErr
		}
	}
	body := map[string]any{
		"plan":  plan,
		"ready": err == nil,
	}
	if err == nil {
		body["ddl"] = ddlText
	} else {
		body["message"] = err.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

// handleMigrateChange 把迁移预检出来的建表语句做成一张结构预案（不执行）。
//
// **刻意不接受客户端传来的 DDL**：语句由服务端按 decisions 重新计算。客户端
// 只能说「这一列用哪种目标类型」，不能说「执行这句话」——否则它就绕过了
// 类型白名单与预检本身。
//
// 生成的预案走已有的 /changes/{id}/apply：审批、审计、竞态保护都复用，
// 不为迁移另开一条执行路径。
func (s *Server) handleMigrateChange(w http.ResponseWriter, r *http.Request) {
	var req migratePlanRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}
	if req.Source == "" || req.Target == "" || req.Table == "" {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
			"需要 source、target、table", "", false)
		return
	}

	s.mu.RLock()
	src, sok := s.conns[req.Source]
	dst, tok := s.conns[req.Target]
	s.mu.RUnlock()
	if !sok || !tok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection",
			"源或目标连接不存在", "", false)
		return
	}
	if src.exec == nil || dst.exec == nil {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"迁移只支持关系型连接", "", false)
		return
	}

	_, ddlText, err := service.PlanMigrationBetween(r.Context(), src.exec, dst.exec, req.Table, req.TargetTable, req.Decisions)
	if errors.Is(err, service.ErrMigrateNeedsDecision) {
		writeError(w, http.StatusBadRequest, "MIGRATE_NEEDS_DECISION", "internal",
			err.Error(), "逐列给出 decisions（列名 → 目标类型）后重试", false)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "MIGRATE_PLAN_FAILED", "internal", err.Error(), "", true)
		return
	}

	name := req.TargetTable
	if name == "" {
		name = req.Table
	}
	plan, err := dst.exec.BuildDDLPlan(r.Context(), service.DDLRequest{
		Table:  model.ObjectRef{Kind: model.KindTable, Name: name},
		Action: service.DDLRaw,
		RawSQL: ddlText,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "MIGRATE_CHANGE_FAILED", "internal", err.Error(), "", false)
		return
	}

	s.mu.Lock()
	s.purgeExpiredPlansLocked()
	s.plans[plan.ID] = &storedPlan{plan: plan, exec: dst.exec, exp: plan.ExpiresAt}
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, plan)
}

// migrateRunRequest 是数据搬运的请求体。
type migrateRunRequest struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Table  string `json:"table"`
	// TargetTable 省略时与 Table 相同。
	TargetTable string `json:"target_table"`
	// BatchSize 默认 1000。
	BatchSize int `json:"batch_size"`
	// MaxRows > 0 时只搬这么多行（演练）。
	MaxRows int `json:"max_rows"`
}

// handleMigrateRun 搬数据并校验（目标表必须已存在）。
//
// 结构由 /migrate/change + /changes/{id}/apply 建好之后再调这里。检查点续跑
// 暂时只做服务层与 CLI（HTTP 侧没有可靠的本地路径语义），这里不接 Resume。
func (s *Server) handleMigrateRun(w http.ResponseWriter, r *http.Request) {
	var req migrateRunRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal", err.Error(), "", false)
		return
	}
	if req.Source == "" || req.Target == "" || req.Table == "" {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", "internal",
			"需要 source、target、table", "", false)
		return
	}

	s.mu.RLock()
	src, sok := s.conns[req.Source]
	dst, tok := s.conns[req.Target]
	s.mu.RUnlock()
	if !sok || !tok {
		writeError(w, http.StatusNotFound, "CONNECTION_NOT_FOUND", "connection",
			"源或目标连接不存在", "", false)
		return
	}
	if src.exec == nil || dst.exec == nil {
		writeError(w, http.StatusBadRequest, "CAPABILITY_UNSUPPORTED", "internal",
			"迁移只支持关系型连接", "", false)
		return
	}

	rep, err := service.MigrateTable(r.Context(), src.exec, dst.exec, req.Table, req.TargetTable,
		service.MigrateExecOptions{BatchSize: req.BatchSize, MaxRows: req.MaxRows})
	if errors.Is(err, service.ErrMigrateVerify) {
		writeError(w, http.StatusConflict, "MIGRATE_VERIFY_FAILED", "internal",
			err.Error(), "目标数据与源不一致：请核对后清空目标表重搬，不要直接使用这份数据", false)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "MIGRATE_RUN_FAILED", "internal", err.Error(), "", false)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
