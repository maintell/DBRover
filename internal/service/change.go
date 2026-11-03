package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
)

var (
	// ErrNoPrimaryKeyForEdit 表示表没有主键，无法安全定位行。
	ErrNoPrimaryKeyForEdit = errors.New("该表没有主键，无法安全定位行，禁止编辑")
	// ErrRowConflict 表示按原值定位不到行（被他人改过或删掉了）。
	ErrRowConflict = errors.New("行已被其他会话修改或删除")
	// ErrPlanExpired 表示预案超过有效期。
	ErrPlanExpired = errors.New("变更预案已过期，请重新生成")
)

// ChangeOp 是单行的变更类型。
type ChangeOp string

const (
	OpInsert ChangeOp = "insert"
	OpUpdate ChangeOp = "update"
	OpDelete ChangeOp = "delete"
)

// RowChange 是改动描述。
//
// Before 只在 update/delete 时需要：它既用于乐观并发校验（拼进 WHERE），
// 也用于生成回滚语句。没有它就等于「盲写」，那是数据事故的主要来源。
type RowChange struct {
	Op     ChangeOp       `json:"op"`
	Key    map[string]any `json:"key,omitempty"`
	Before map[string]any `json:"before,omitempty"`
	After  map[string]any `json:"after,omitempty"`
}

// PlanStatement 是一条待执行语句。
type PlanStatement struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args,omitempty"`
	// ExpectRows 是期望影响行数；-1 表示不校验。
	ExpectRows int `json:"expect_rows"`
}

// ChangePlan 是一次数据变更的预案（ADR-0003）。
type ChangePlan struct {
	ID         string          `json:"id"`
	Table      model.ObjectRef `json:"table"`
	Statements []PlanStatement `json:"statements"`
	Rollback   []PlanStatement `json:"rollback"`
	Risk       policy.Level    `json:"risk"`
	Friction   policy.Friction `json:"friction"`
	// Irreversible 表示无法生成回滚（例如插入用了自增主键且未回填）。
	Irreversible bool      `json:"irreversible"`
	ExpiresAt    time.Time `json:"expires_at"`
	CreatedAt    time.Time `json:"created_at"`
	// Undo 表示这是一张撤销预案（由刚执行成功的预案反向生成）。
	// 用它挡住「撤销的撤销」——否则同一个变更可以被无限来回翻。
	Undo bool `json:"undo,omitempty"`
	// DDL 表示这是一次结构变更，而不是行数据变更。
	DDL bool `json:"ddl,omitempty"`
	// RollsBackAsCompensation 为 true 时 Rollback 只是补偿语句，不是真正的撤销：
	// 引擎的 DDL 隐式提交（MySQL / Oracle），已经发生的结构变化撤不回来。
	RollsBackAsCompensation bool `json:"rolls_back_as_compensation,omitempty"`
	// AffectedRows 是影响行数；-1 表示未知（不假装是 0）。
	AffectedRows int64 `json:"affected_rows"`
	// ExpectDDLHash 是生成预案时看到的表定义哈希，执行前比对。
	// 只在服务端保管，不对外暴露（客户端不需要，也不该能改）。
	ExpectDDLHash string `json:"-"`
	// ExpectTableAbsent 为 true 时，执行前要求目标表仍然不存在。
	// 这是建表预案的竞态保护：别人先建了同名表，就不该再按计划去建。
	ExpectTableAbsent bool `json:"-"`
}

// ChangeSetRequest 是构建预案的输入。
type ChangeSetRequest struct {
	ConnectionID string          `json:"connection_id"`
	Table        model.ObjectRef `json:"table"`
	Changes      []RowChange     `json:"changes"`
}

const planTTL = 10 * time.Minute

// BuildChangePlan 把变更集编译成预案：语句、回滚、风险、有效期。
func (e *Executor) BuildChangePlan(ctx context.Context, req ChangeSetRequest) (*ChangePlan, error) {
	if len(req.Changes) == 0 {
		return nil, errors.New("变更集为空")
	}

	td, err := e.conn.Meta().DescribeTable(ctx, req.Table)
	if err != nil {
		return nil, err
	}
	pk := td.PrimaryKeyColumns()
	if len(pk) == 0 {
		return nil, ErrNoPrimaryKeyForEdit
	}

	cols := map[string]model.Column{}
	for _, c := range td.Columns {
		cols[strings.ToLower(c.Name)] = c
	}

	table := e.dialect.QuoteQualified(schemaOf(req.Table, e.cfgSchema()), req.Table.Name)

	plan := &ChangePlan{
		ID:        "plan-" + newID(),
		Table:     req.Table,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(planTTL),
	}

	var highest policy.Level
	for _, ch := range req.Changes {
		fwd, back, reversible, err := e.compileRowChange(table, pk, cols, ch)
		if err != nil {
			return nil, err
		}
		plan.Statements = append(plan.Statements, fwd...)
		// 回滚要反序：先撤后做的改动。
		plan.Rollback = append(back, plan.Rollback...)
		if !reversible {
			plan.Irreversible = true
		}

		for _, st := range fwd {
			g := policy.GradeStatement(e.dialect, st.SQL, nil)
			if g.Level > highest {
				highest = g.Level
			}
		}
	}

	plan.Risk = highest
	// 变更集的行数是确切的（就是想改这几行），不能用「未知」的修正项。
	// 变更集自带语句预览，因此摩擦比同等级原始 SQL 低一级（ADR-0004）。
	plan.Friction = policy.FrictionFor(e.env, highest, policy.Modifiers{
		ChangeSetPath:      true,
		StatementCount:     len(plan.Statements),
		RowsAffected:       int64(len(plan.Statements)),
		ReadOnlyConnection: !e.mode.AllowsWrite(),
	})
	return plan, nil
}

// compileRowChange 生成单行的正向与回滚语句。
func (e *Executor) compileRowChange(table string, pk []string, cols map[string]model.Column, ch RowChange) (
	fwd, back []PlanStatement, reversible bool, err error) {

	reversible = true

	switch ch.Op {
	case OpUpdate:
		if len(ch.After) == 0 {
			return nil, nil, true, errors.New("update 缺少新值")
		}
		if err := requireKeys(ch, pk); err != nil {
			return nil, nil, true, err
		}

		setCols := sortedKeys(ch.After)
		assigns := make([]string, 0, len(setCols))
		args := make([]any, 0, len(setCols)+len(pk)*2)
		for _, c := range setCols {
			assigns = append(assigns, e.dialect.QuoteIdent(c)+" = ?")
			args = append(args, ch.After[c])
		}
		where, wargs := e.optimisticWhere(pk, ch)
		args = append(args, wargs...)

		fwd = []PlanStatement{{
			SQL:  "UPDATE " + table + " SET " + strings.Join(assigns, ", ") + " WHERE " + where,
			Args: args, ExpectRows: 1,
		}}

		// 回滚：把 Before 里被改动的列写回去。
		backAssigns := make([]string, 0, len(setCols))
		backArgs := make([]any, 0, len(setCols)+len(pk))
		for _, c := range setCols {
			backAssigns = append(backAssigns, e.dialect.QuoteIdent(c)+" = ?")
			backArgs = append(backArgs, ch.Before[c])
		}
		backArgs = append(backArgs, keyArgs(pk, ch.Key)...)
		back = []PlanStatement{{
			SQL:  "UPDATE " + table + " SET " + strings.Join(backAssigns, ", ") + " WHERE " + pkWhere(e, pk),
			Args: backArgs, ExpectRows: 1,
		}}

	case OpDelete:
		if err := requireKeys(ch, pk); err != nil {
			return nil, nil, true, err
		}
		where, wargs := e.optimisticWhere(pk, ch)
		fwd = []PlanStatement{{
			SQL:  "DELETE FROM " + table + " WHERE " + where,
			Args: wargs, ExpectRows: 1,
		}}

		// 回滚：把整行插回去。缺列就视为不可逆——不能假装能还原。
		if len(ch.Before) == 0 {
			reversible = false
			break
		}
		names := sortedKeys(ch.Before)
		marks := make([]string, len(names))
		vals := make([]any, len(names))
		for i, n := range names {
			marks[i] = "?"
			vals[i] = ch.Before[n]
		}
		back = []PlanStatement{{
			SQL: "INSERT INTO " + table + " (" + strings.Join(e.quoteAll(names), ", ") +
				") VALUES (" + strings.Join(marks, ", ") + ")",
			Args: vals, ExpectRows: 1,
		}}

	case OpInsert:
		if len(ch.After) == 0 {
			return nil, nil, true, errors.New("insert 缺少列值")
		}
		names := sortedKeys(ch.After)
		marks := make([]string, len(names))
		vals := make([]any, len(names))
		for i, n := range names {
			marks[i] = "?"
			vals[i] = ch.After[n]
		}
		fwd = []PlanStatement{{
			SQL: "INSERT INTO " + table + " (" + strings.Join(e.quoteAll(names), ", ") +
				") VALUES (" + strings.Join(marks, ", ") + ")",
			Args: vals, ExpectRows: 1,
		}}

		// 回滚：按主键删除。主键没给（自增）就无法定位，标为不可逆。
		if err := requireKeys(ch, pk); err != nil {
			reversible = false
			break
		}
		back = []PlanStatement{{
			SQL:  "DELETE FROM " + table + " WHERE " + pkWhere(e, pk),
			Args: keyArgs(pk, ch.Key), ExpectRows: 1,
		}}

	default:
		return nil, nil, true, fmt.Errorf("未知变更类型: %s", ch.Op)
	}

	return fwd, back, reversible, nil
}

// optimisticWhere 生成「主键 + 原值」的定位条件。
// 原值用于并发校验：别人改过就匹配不到行，从而报冲突而不是覆盖。
func (e *Executor) optimisticWhere(pk []string, ch RowChange) (string, []any) {
	conds := make([]string, 0, len(pk)*2)
	args := make([]any, 0, len(pk)*2)

	for _, c := range pk {
		conds = append(conds, e.dialect.QuoteIdent(c)+" = ?")
		args = append(args, ch.Key[c])
	}
	// 只对提供了原值的列做校验。
	for _, c := range sortedKeys(ch.Before) {
		if containsFold(pk, c) {
			continue
		}
		if ch.After != nil {
			if _, ok := ch.After[c]; !ok {
				continue // 这一列没被改，不必校验
			}
		}
		v := ch.Before[c]
		if v == nil {
			conds = append(conds, e.dialect.QuoteIdent(c)+" IS NULL")
			continue
		}
		conds = append(conds, e.dialect.QuoteIdent(c)+" = ?")
		args = append(args, v)
	}
	return strings.Join(conds, " AND "), args
}

func (e *Executor) quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = e.dialect.QuoteIdent(n)
	}
	return out
}

func (e *Executor) cfgSchema() string {
	if c, ok := e.conn.(interface{ Schema() string }); ok {
		return c.Schema()
	}
	return ""
}

// ApplyChangePlan 执行预案。摩擦与审批沿用统一执行门。
func (e *Executor) ApplyChangePlan(ctx context.Context, plan *ChangePlan, req Request) (*Outcome, error) {
	started := time.Now()
	actor := actorOf(req.Actor)
	out := &Outcome{ExecutionID: newID()}

	if time.Now().After(plan.ExpiresAt) {
		out.Blocked = true
		out.Reason = ErrPlanExpired.Error()
		return out, nil
	}

	out.Risk = plan.Risk
	out.Friction = plan.Friction
	out.StatementHash = plan.ID

	// 结构变更的前置条件：表定义在生成预案后被动过，就不能再按旧结构执行。
	// 数据变更靠乐观并发校验行，DDL 没有行可供校验，只能比对整表定义。
	if plan.ExpectDDLHash != "" {
		cur, err := e.conn.Meta().DDL(ctx, plan.Table)
		if err == nil && ddlHash(cur) != plan.ExpectDDLHash {
			out.Err = &ExecError{
				Code: "SCHEMA_CHANGED", Category: "constraint",
				Message: ErrSchemaChanged.Error(),
				Hint:    "重新打开结构页生成新的预案",
			}
			out.Duration = time.Since(started)
			e.record(actor, req, out, "error", plan.Table.Name, out.Err.Message)
			return out, nil
		}
	}
	// 建表的竞态保护：别人已经建了同名表就停手，别去撞引擎的 already exists。
	if plan.ExpectTableAbsent {
		if exists, err := e.tableExists(ctx, plan.Table); err == nil && exists {
			out.Err = &ExecError{
				Code: "SCHEMA_CHANGED", Category: "constraint",
				Message: "表已存在，建表预案作废",
				Hint:    "刷新表清单后重试",
			}
			out.Duration = time.Since(started)
			e.record(actor, req, out, "error", plan.Table.Name, out.Err.Message)
			return out, nil
		}
	}

	if plan.Friction >= policy.F2 && !req.Approval {
		out.ApprovalRequired = true
		out.Blocked = true
		out.Reason = fmt.Sprintf("%s 上的 %s 级变更需要确认（当前 %s）", e.env, plan.Risk, plan.Friction)
		out.Duration = time.Since(started)
		e.record(actor, req, out, "denied", plan.Table.Name, out.Reason)
		return out, nil
	}

	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return e.fail(req, out, actor, started, plan.Risk, plan.Friction, err)
	}
	defer func() { _ = sess.Close() }()

	// 单事务执行，任何一条不符合预期就整体回滚。
	// 开不出事务就中止：降级成「逐条自动提交」会在中途失败时留下半改状态，
	// 那正是变更集要避免的事。
	if err := sess.Begin(ctx); err != nil {
		return e.fail(req, out, actor, started, plan.Risk, plan.Friction, err)
	}

	rollbackAll := func() {
		_ = sess.Rollback(ctx)
	}

	for i, st := range plan.Statements {
		res, err := sess.Execute(ctx, st.SQL, st.Args)
		if err != nil {
			rollbackAll()
			out.Err = ClassifyError(err)
			out.Duration = time.Since(started)
			e.record(actor, req, out, "error", st.SQL, err.Error())
			return out, nil
		}
		if st.ExpectRows >= 0 && res.RowsAffected != int64(st.ExpectRows) {
			rollbackAll()
			out.Err = &ExecError{
				Code: "CHANGE_ROW_CONFLICT", Category: "constraint",
				Message:   ErrRowConflict.Error(),
				Hint:      fmt.Sprintf("第 %d 条语句影响 %d 行，期望 %d 行", i+1, res.RowsAffected, st.ExpectRows),
				Retryable: false,
			}
			out.Duration = time.Since(started)
			e.record(actor, req, out, "error", st.SQL, out.Err.Message)
			return out, nil
		}
	}

	if err := sess.Commit(ctx); err != nil {
		out.Err = ClassifyError(err)
		out.Duration = time.Since(started)
		e.record(actor, req, out, "error", "commit", err.Error())
		return out, nil
	}

	out.Duration = time.Since(started)
	e.record(actor, req, out, "ok", plan.Table.Name, "")
	return out, nil
}

func requireKeys(ch RowChange, pk []string) error {
	for _, c := range pk {
		if _, ok := ch.Key[c]; !ok {
			return fmt.Errorf("缺少主键列 %s", c)
		}
	}
	return nil
}

func keyArgs(pk []string, key map[string]any) []any {
	out := make([]any, len(pk))
	for i, c := range pk {
		out[i] = key[c]
	}
	return out
}

func pkWhere(e *Executor, pk []string) string {
	parts := make([]string, len(pk))
	for i, c := range pk {
		parts[i] = e.dialect.QuoteIdent(c) + " = ?"
	}
	return strings.Join(parts, " AND ")
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// schemaOf 选默认模式：对象自带优先，其次连接配置。
func schemaOf(ref model.ObjectRef, fallback string) string {
	if ref.Schema != "" {
		return ref.Schema
	}
	return fallback
}
