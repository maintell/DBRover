// Package service 是业务编排层，也是权限判断的唯一位置。
//
// 所有写操作必须经过本层的执行门：切分 → 风险分级 → 摩擦计算 →
// 只读拦截 → 审批校验 → 执行 → 审计。
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/dialect"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
)

// AuditEvent 是一条不可变执行记录。
type AuditEvent struct {
	ID         string          `json:"id"`
	At         time.Time       `json:"at"`
	Actor      string          `json:"actor"`  // human | agent | schedule | system
	Action     string          `json:"action"` // query.execute | change.apply | ...
	Target     string          `json:"target,omitempty"`
	Risk       policy.Level    `json:"risk"`
	Friction   policy.Friction `json:"friction"`
	Outcome    string          `json:"outcome"` // ok | denied | error
	SQL        string          `json:"sql,omitempty"`
	DurationMS int64           `json:"duration_ms"`
	Reason     string          `json:"reason,omitempty"`

	// PrevHash / Hash 构成链式哈希（docs/10 §6.2）：
	// hash = SHA256(prev_hash || canonical_json(event_without_hash))。
	//
	// 它防的是「本地文件被悄悄编辑」：改一行，后面整条链都对不上。
	// 不防「有管理员权限的人整段重写」——那需要外部锚点，v1 不做。
	PrevHash string `json:"prev_hash,omitempty"`
	Hash     string `json:"hash,omitempty"`
}

// AuditSink 接收审计事件。实现必须只追加。
type AuditSink interface {
	Record(ev AuditEvent)
}

// Request 是一次执行请求。
type Request struct {
	SQL  string
	Args []any
	// Cursor 是光标在 SQL 文本里的字节偏移；非 nil 时只执行光标所在的那条语句。
	//
	// 用指针而不是「负数表示未设置」：零值必须是「整段脚本」，
	// 否则任何没设 Cursor 的调用方（脚本、测试、CLI）都会静默变成只跑第一条。
	//
	// 由服务端来挑，而不是让前端自己切：切分要认 dollar-quote、DELIMITER、
	// 字符串里的分号，前端重写一遍必然与引擎侧行为分叉。前端只提供偏移量。
	Cursor *int
	// Approval 表示调用方已就人工确认取得的凭证。
	// F2 及以上的操作缺少该凭证时会被拒绝执行。
	Approval bool
	// StatementHash 是调用方声称已确认的语句指纹。
	// 提供时必须与本次 SQL 的指纹一致，否则拒绝（防「审批 A、执行 B」）。
	StatementHash string
	// ChangeSetPath 表示经由变更集提交（路径自带语句预览），摩擦降一级。
	ChangeSetPath bool
	// MaxRows 限制单条查询返回的最大行数，0 表示默认 1000，封顶 10 万（docs/05 §8）。
	MaxRows int
	// FetchSize 是驱动侧批大小，0 表示默认 200。
	FetchSize int
	// ContinueOnError 为 true 时，脚本里某条语句失败后继续跑后面的（docs/05 §2.1
	// 的「继续执行」，即 stop_on_error=false）。
	//
	// 零值是「遇错即停」：多语句脚本里第一条失败通常说明前提不成立，闷头往下跑
	// 更容易把库改坏。要「继续执行」必须显式要求。
	ContinueOnError bool
	// Actor 是发起者标识，默认 human:local。
	Actor string
}

// StatementResult 是单条语句的结果。
type StatementResult struct {
	Index      int            `json:"index"`
	SQL        string         `json:"sql"`
	Grade      policy.Grade   `json:"grade"`
	Columns    []model.Column `json:"columns,omitempty"`
	Rows       [][]any        `json:"rows,omitempty"`
	Affected   int64          `json:"affected"`
	DurationMS int64          `json:"duration_ms"`
	Err        string         `json:"err,omitempty"`
}

// Outcome 是一次执行的结果。
type Outcome struct {
	ExecutionID string          `json:"execution_id"`
	Risk        policy.Level    `json:"risk"`
	Friction    policy.Friction `json:"friction"`
	Blocked     bool            `json:"blocked"`
	Reason      string          `json:"reason,omitempty"`
	// BlockCode / BlockHint 让「被拦下的具体原因」变成稳定错误码，而不是靠
	// Friction 反推。只有具体预检会设置；为空时调用方按原有规则分类。
	BlockCode string            `json:"block_code,omitempty"`
	BlockHint string            `json:"block_hint,omitempty"`
	Results   []StatementResult `json:"results,omitempty"`
	Duration  time.Duration     `json:"duration_ms"`
	// ApprovalRequired 表示被拦下的原因是「需要人工确认」。
	// 调用方应据此签发绑定 StatementHash 的审批凭证。
	ApprovalRequired bool `json:"approval_required"`
	// StatementHash 是本次 SQL 的指纹，审批凭证必须绑定它。
	StatementHash string `json:"statement_hash,omitempty"`
	// Err 是执行期错误的结构化表示。非 nil 时调用方应返回非 2xx。
	Err *ExecError `json:"error,omitempty"`
	// RowsAffected 是本次实际提交的行数。分批执行中途失败或中止时，
	// 它是「已经落到库里、撤不回来」的那部分，必须如实报出来。
	RowsAffected int64 `json:"rows_affected"`
	// Aborted 表示执行被调用方中止（取消 ctx），不是失败也不是完成。
	// 中止后已提交的批留在库里，预案仍然有效，可以重新提交改完剩下的行。
	Aborted bool `json:"aborted,omitempty"`
}

// Executor 是执行门。
type Executor struct {
	conn    driver.Connector
	dialect dialect.Dialect
	env     model.Environment
	mode    model.AccessMode
	audit   AuditSink
	// slow 保存上一次慢查询采样的累计值，用来算窗口增量（docs/09 §6.2）。
	// 每条连接一个 Executor，所以按连接天然隔离。
	slow slowSampler
	// qps 保存上一次的累计语句数，用来算 QPS（同样需要两次采样）。
	qps qpsSampler
	// alerts 保存「连续几次」与抑制窗口（FR-MON-08，只在运行时生效）。
	alerts alertState
	// WarmRelations 为 true 时，执行前会查目标表主键以精确分级到 R1。
	WarmRelations bool
	// cursors 持有流式查询的游标，生命周期由执行器管理。
	cursors *cursorStore
	// runMu / running 跟踪「此刻在跑的语句」，供取消使用（见 cancel.go）。
	runMu   sync.Mutex
	running map[*runningExec]struct{}
}

// NewExecutor 构造执行门。
func NewExecutor(conn driver.Connector, env model.Environment, mode model.AccessMode, audit AuditSink) *Executor {
	if env == "" {
		env = model.EnvUnknown
	}
	if mode == "" {
		mode = model.DefaultAccessMode(env)
	}
	return &Executor{
		conn:          conn,
		dialect:       dialect.For(conn.Engine()),
		env:           env,
		mode:          mode,
		audit:         audit,
		WarmRelations: true,
		cursors:       newCursorStore(defaultCursorTTL),
	}
}

type prepared struct {
	text string
	an   dialect.Analysis
	gr   policy.Grade
}

// Execute 执行请求。返回的 Outcome 即使 Blocked 也不返回 error ——
// 被策略拒绝是一次正常结果，不是异常。
func (e *Executor) Execute(ctx context.Context, req Request) (*Outcome, error) {
	started := time.Now()
	actor := req.Actor
	if actor == "" {
		actor = "human:local"
	}
	out := &Outcome{ExecutionID: newID()}

	// 1. 切分。切分失败说明脚本本身不可信，直接拒绝。
	stmts, err := e.dialect.Split(req.SQL)
	if err != nil {
		return e.deny(req, out, actor, started, policy.R4, policy.F5, "脚本切分失败: "+err.Error())
	}
	if len(stmts) == 0 {
		return nil, errors.New("没有可执行的语句")
	}

	// 1.5 光标定位：只跑光标所在的那一条（docs/05 验收 1、2）。
	// 指纹仍按整段原文算——执行的是被批准文本的子集，不会放大权限。
	if req.Cursor != nil {
		stmts = []dialect.Statement{statementAt(stmts, *req.Cursor)}
	}

	// 1.6 跨库引用预检（docs/05 验收 12）。
	//
	// PostgreSQL 一个连接只对应一个库，`库.模式.表` 三段名引擎必然报
	// "cross-database references are not implemented"。在这里拦下并说清
	// 该怎么办，比把引擎的原始报错丢给用户有用。
	for _, st := range stmts {
		ref, crossDB := e.dialect.CrossDatabase(st.Text)
		if !crossDB {
			continue
		}
		out.Blocked = true
		out.BlockCode = "CROSS_DATABASE_UNSUPPORTED"
		out.BlockHint = "PostgreSQL 的一个连接只能访问当前库：请另开一个连接到该库；" +
			"确需在一条语句里跨库，可安装 postgres_fdw 或 dblink"
		out.Reason = fmt.Sprintf("不支持跨库引用 %s：这个连接只能访问当前库", ref)
		out.Risk = policy.R0
		out.Friction = policy.F0
		out.Duration = time.Since(started)
		e.record(actor, req, out, "denied", req.SQL, out.Reason)
		return out, nil
	}

	// 2. 全量预检：任何一条被拒，整批都不执行，避免半途生效。
	preps, maxLevel, anyWrite := e.gradeAll(ctx, stmts)

	// 3. 只读连接上的写操作直接拒绝。未知模式按不可写处理（失败关闭）。
	if anyWrite && !e.mode.AllowsWrite() {
		return e.deny(req, out, actor, started, maxLevel, policy.F5, "连接为只读访问模式，写入被拒绝")
	}

	// 4. 摩擦计算。
	friction := e.frictionFor(maxLevel, anyWrite, req, len(stmts))
	out.Risk = maxLevel
	out.Friction = friction

	// 5. 审批校验。F2 及以上必须有审批凭证，且凭证必须绑定本次语句内容。
	stmtHash, _ := e.dialect.Fingerprint(req.SQL)
	out.StatementHash = stmtHash

	if req.Approval && req.StatementHash != "" && stmtHash != "" && req.StatementHash != stmtHash {
		out.Blocked = true
		out.Risk = maxLevel
		out.Friction = friction
		out.Reason = "审批内容与实际语句不一致（语句指纹不匹配）"
		out.Duration = time.Since(started)
		e.record(actor, req, out, "denied", req.SQL, out.Reason)
		return out, nil
	}

	if friction >= policy.F2 && !req.Approval {
		out.ApprovalRequired = true
		return e.deny(req, out, actor, started, maxLevel, friction,
			fmt.Sprintf("%s 上的 %s 级操作需要确认（当前 %s）", e.env, maxLevel, friction))
	}

	// 6. 执行。
	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return e.fail(req, out, actor, started, maxLevel, friction, err)
	}
	defer func() { _ = sess.Close() }()

	// 登记「这一条在跑」：用户按取消时，取消的就是它。
	runCtx, doneRun := e.beginRun(ctx, sess)
	defer doneRun()
	ctx = runCtx

	maxRows := clampMaxRows(req.MaxRows)

	for i, p := range preps {
		sr := StatementResult{Index: i, SQL: p.text, Grade: p.gr}
		t0 := time.Now()
		// 必须留住 error 本身而不是它的字符串：ClassifyError 认的是
		// context.Canceled / 驱动哨兵这类**身份**，包装成新 error 之后就只剩
		// 一句 "context canceled"，取消会被归成 SQL_EXEC_FAILED。
		var stmtErr error

		if p.an.Class == dialect.ClassQuery && !p.an.SelectInto && !p.an.WriteInCTE {
			cur, err := sess.Query(ctx, p.text, req.Args, driver.QueryOptions{MaxRows: maxRows})
			if err != nil {
				stmtErr = err
			} else {
				sr.Columns = cur.Columns()
				for {
					rows, more, err := cur.Next(ctx)
					if err != nil {
						stmtErr = err
						break
					}
					sr.Rows = append(sr.Rows, rows...)
					if !more {
						break
					}
				}
				_ = cur.Close()
			}
		} else {
			res, err := sess.Execute(ctx, p.text, req.Args)
			if err != nil {
				stmtErr = err
			} else {
				sr.Affected = res.RowsAffected
			}
		}

		if stmtErr != nil {
			sr.Err = stmtErr.Error()
		}
		sr.DurationMS = time.Since(t0).Milliseconds()
		out.Results = append(out.Results, sr)
		if stmtErr == nil {
			continue
		}
		// 第一条失败就是这一批的结论：后面的语句再多成功，也不改变「脚本没跑通」。
		if out.Err == nil {
			out.Err = ClassifyError(stmtErr)
		}
		// 遇错即停是默认（docs/05 §2.1）：已成功的语句保留结果，后面的不执行。
		// 显式要求「继续执行」时才往下走，每条结果独立。
		if !req.ContinueOnError {
			out.Duration = time.Since(started)
			e.record(actor, req, out, "error", p.text, sr.Err)
			return out, nil
		}
	}

	out.Duration = time.Since(started)
	outcome := "ok"
	if out.Err != nil {
		outcome = "error"
	}
	e.record(actor, req, out, outcome, req.SQL, "")
	return out, nil
}

// statementAt 挑出光标所在的那条语句。
//
// 规则来自编辑器里的实际习惯（docs/05 验收 1、2）：
//   - 光标在某条语句内部 → 就是那一条；
//   - 光标落在两条语句之间（空白 / 分号后 / 下一行行首）→ 前一条。
//     写完一句按回车，光标停在下一行，此时按 Ctrl+Enter 想跑的就是刚写完的那句；
//   - 光标在第一句之前 → 第一句；
//   - 光标超出文本末尾 → 最后一句。
//
// 不做「取最近的语句」这种更聪明的判断：光标离哪条近是视觉直觉，不是文本事实，
// 猜错一次就会执行到用户没打算跑的那条。
func statementAt(stmts []dialect.Statement, cursor int) dialect.Statement {
	for i := len(stmts) - 1; i >= 0; i-- {
		if cursor >= stmts[i].Start {
			return stmts[i]
		}
	}
	return stmts[0]
}

// gradeAll 对整批语句做分级，返回预检结果。
func (e *Executor) gradeAll(ctx context.Context, stmts []dialect.Statement) ([]prepared, policy.Level, bool) {
	rels := e.relations(ctx, stmts)
	preps := make([]prepared, 0, len(stmts))
	var maxLevel policy.Level
	anyWrite := false
	for _, st := range stmts {
		an, _ := e.dialect.Analyze(st.Text)
		gr := policy.GradeStatement(e.dialect, st.Text, rels)
		if gr.Level > maxLevel {
			maxLevel = gr.Level
		}
		if gr.Writes {
			anyWrite = true
		}
		preps = append(preps, prepared{text: st.Text, an: an, gr: gr})
	}
	return preps, maxLevel, anyWrite
}

// frictionFor 计算打扰强度。影响行数未知时按保守处理。
func (e *Executor) frictionFor(maxLevel policy.Level, anyWrite bool, req Request, stmtCount int) policy.Friction {
	mods := policy.Modifiers{
		ChangeSetPath:      req.ChangeSetPath,
		StatementCount:     stmtCount,
		ReadOnlyConnection: !e.mode.AllowsWrite(),
	}
	switch maxLevel {
	case policy.R1:
		// 命中主键等值，影响行数最多 1，无需按「未知」加码。
		mods.RowsAffected = 1
	case policy.R2:
		mods.RowsAffected = -1
	default:
		mods.RowsAffected = 0
	}
	f := policy.FrictionFor(e.env, maxLevel, mods)
	if !anyWrite {
		f = policy.F0
	}
	return f
}

// relations 收集分级所需的主键信息。
func (e *Executor) relations(ctx context.Context, stmts []dialect.Statement) policy.Relations {
	if !e.WarmRelations {
		return nil
	}
	out := policy.StaticRelations{}
	for _, st := range stmts {
		an, err := e.dialect.Analyze(st.Text)
		if err != nil || an.TargetTable == "" {
			continue
		}
		table := an.TargetTable
		if _, done := out[table]; done {
			continue
		}
		td, err := e.conn.Meta().DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: table})
		if err != nil || td == nil {
			continue
		}
		if pk := td.PrimaryKeyColumns(); len(pk) > 0 {
			out[table] = pk
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (e *Executor) deny(req Request, out *Outcome, actor string, started time.Time,
	level policy.Level, f policy.Friction, reason string) (*Outcome, error) {
	out.Blocked = true
	out.Risk = level
	out.Friction = f
	out.Reason = reason
	out.Duration = time.Since(started)
	e.record(actor, req, out, "denied", req.SQL, reason)
	return out, nil
}

func (e *Executor) fail(req Request, out *Outcome, actor string, started time.Time,
	level policy.Level, f policy.Friction, err error) (*Outcome, error) {
	out.Risk = level
	out.Friction = f
	out.Duration = time.Since(started)
	e.record(actor, req, out, "error", req.SQL, err.Error())
	return out, err
}

func (e *Executor) record(actor string, req Request, out *Outcome, outcome, sqlText, reason string) {
	if e.audit == nil {
		return
	}
	e.audit.Record(AuditEvent{
		ID:       out.ExecutionID,
		At:       time.Now(),
		Actor:    actor,
		Action:   "query.execute",
		Target:   string(e.conn.Engine()),
		Risk:     out.Risk,
		Friction: out.Friction,
		Outcome:  outcome,
		// 口令不落审计：这行语句的文本里可能就带着新口令（docs/10 §6.2 能力边界）。
		SQL:        RedactSQL(sqlText),
		DurationMS: out.Duration.Milliseconds(),
		Reason:     reason,
	})
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
