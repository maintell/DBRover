package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
)

// ErrKVCommandBlocked 表示这条命令被策略直接封禁（不给审批机会）。
var ErrKVCommandBlocked = errors.New("该命令被策略封禁")

// KVExecutor 是 key-value 引擎的执行门。
//
// 它与 Executor（SQL）同构：风险分级 → 摩擦 → 审批 → 审计。分开实现而不是
// 塞进 Executor，因为「一条 Redis 命令」与「一段 SQL」在可分析性上差太远：
// SQL 要过词法与语句分类，命令只需要查表（ADR-0007 §3）。
type KVExecutor struct {
	kv    driver.KVConnector
	env   model.Environment
	mode  model.AccessMode
	audit AuditSink
}

// NewKVExecutor 构造 key-value 执行门。
func NewKVExecutor(kv driver.KVConnector, env model.Environment, mode model.AccessMode, audit AuditSink) *KVExecutor {
	if env == "" {
		env = model.EnvUnknown
	}
	if mode == "" {
		mode = model.DefaultAccessMode(env)
	}
	return &KVExecutor{kv: kv, env: env, mode: mode, audit: audit}
}

// KVCommandRequest 是一次命令模式的执行请求。
type KVCommandRequest struct {
	Name string
	Args []any
	// Approval 表示调用方已就人工确认取得凭证。
	Approval bool
	// StatementHash 是调用方声称已确认的指纹，必须与本次命令一致。
	StatementHash string
}

// KVMutationRequest 是一次结构化写操作的执行请求。
type KVMutationRequest struct {
	Op            driver.KVMutation
	Approval      bool
	StatementHash string
}

// KVOutcome 是 key-value 执行门的结果。
type KVOutcome struct {
	Risk             policy.Level    `json:"risk"`
	Friction         policy.Friction `json:"friction"`
	Blocked          bool            `json:"blocked"`
	ApprovalRequired bool            `json:"approval_required"`
	// ReadOnly 表示被拦下的原因是连接只读（调用方据此映射成 POLICY_READ_ONLY，
	// 而不是靠字符串里有没有「只读」两个字去猜）。
	ReadOnly bool   `json:"read_only,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// StatementHash 是命令指纹，审批凭证必须绑定它。
	StatementHash string           `json:"statement_hash,omitempty"`
	Result        *driver.KVResult `json:"result,omitempty"`
	Err           *ExecError       `json:"error,omitempty"`
}

// CommandFingerprint 是命令的指纹（命令名 + 参数），审批凭证绑定它。
func CommandFingerprint(name string, args []any) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, strings.ToUpper(strings.TrimSpace(name)))
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a))
	}
	return strings.Join(parts, "\x1f")
}

// MutationFingerprint 是写操作的指纹。
func MutationFingerprint(op driver.KVMutation) string {
	return fmt.Sprintf("%s\x1f%s\x1f%s\x1f%s", op.Kind, op.Key, op.Field, op.Member)
}

// mutationCommandName 把结构化写操作映射到「用来分级的那条命令」。
//
// 分级表是按命令名写的，结构化写操作必须先落到一个真实命令上，
// 否则就得为它单独维护第二张表（两份判断迟早会分叉）。
func mutationCommandName(kind driver.KVMutationKind) string {
	switch kind {
	case driver.KVMutSetString:
		return "SET"
	case driver.KVMutSetField:
		return "HSET"
	case driver.KVMutDeleteKey:
		return "UNLINK"
	case driver.KVMutDeleteItem:
		return "HDEL"
	case driver.KVMutPush:
		return "RPUSH"
	case driver.KVMutExpire:
		return "EXPIRE"
	default:
		return "" // 未知写操作按「不在表里」处理 → R4
	}
}

// gate 是两种请求共用的那道门：分级、摩擦、审批。
func (e *KVExecutor) gate(name string, fingerprint string, approval bool, claimedHash string) (*KVOutcome, bool) {
	out := &KVOutcome{StatementHash: fingerprint}

	if reason, blocked := policy.RedisCommandBlocked(name); blocked {
		out.Blocked = true
		out.Risk = policy.R4
		out.Reason = reason
		return out, false
	}

	risk, _ := policy.RedisCommandRisk(name)
	out.Risk = risk
	out.Friction = policy.FrictionFor(e.env, risk, policy.Modifiers{
		StatementCount:     1,
		RowsAffected:       1, // 单 key 操作，行数是确定的
		ReadOnlyConnection: !e.mode.AllowsWrite(),
	})

	// 审批凭证必须绑定这一条命令：拿 A 的凭证执行 B 是最典型的绕过。
	if claimedHash != "" && claimedHash != fingerprint {
		out.Blocked = true
		out.Reason = "审批凭证与当前命令不匹配"
		return out, false
	}
	if out.Friction.Blocked() {
		out.Blocked = true
		if !e.mode.AllowsWrite() {
			out.ReadOnly = true
			out.Reason = fmt.Sprintf("只读连接拒绝 %s 级命令（需要读写连接）", risk)
		} else {
			out.Reason = fmt.Sprintf("%s 上的 %s 级命令被拒绝（当前 %s）", e.env, risk, out.Friction)
		}
		return out, false
	}
	if out.Friction.RequiresApproval() && !approval {
		out.Blocked = true
		out.ApprovalRequired = true
		out.Reason = fmt.Sprintf("%s 上的 %s 级命令需要确认（当前 %s）", e.env, risk, out.Friction)
		return out, false
	}
	return out, true
}

// ExecuteCommand 执行一条命令模式的命令。
func (e *KVExecutor) ExecuteCommand(ctx context.Context, req KVCommandRequest, actor string) *KVOutcome {
	name := strings.ToUpper(strings.TrimSpace(req.Name))
	if name == "" {
		return &KVOutcome{Err: &ExecError{Code: "REQUEST_INVALID", Category: "internal", Message: "缺少命令名"}}
	}
	fp := CommandFingerprint(name, req.Args)
	out, proceed := e.gate(name, fp, req.Approval, req.StatementHash)
	if !proceed {
		e.record(actor, "kv.command", name, out)
		return out
	}

	// 驱动不做读写判断（它没有风险表），所以「只读连接上不许写」这条
	// 必须在门这里落实：风险 ≥ R2 且连接只读，已经不满足 gate 的 F5，
	// 走不到这里；这条注释是为了让后来人别把 F5 那一步删掉。
	res, err := e.kv.Command(ctx, name, req.Args)
	if err != nil {
		out.Err = ClassifyError(err)
		e.record(actor, "kv.command", name, out)
		return out
	}
	out.Result = res
	e.record(actor, "kv.command", name, out)
	return out
}

// ApplyMutation 执行一次结构化写操作。
func (e *KVExecutor) ApplyMutation(ctx context.Context, req KVMutationRequest, actor string) *KVOutcome {
	cmd := mutationCommandName(req.Op.Kind)
	fp := MutationFingerprint(req.Op)
	out, proceed := e.gate(cmd, fp, req.Approval, req.StatementHash)
	if !proceed {
		e.record(actor, "kv.mutate", string(req.Op.Kind), out)
		return out
	}
	res, err := e.kv.Mutate(ctx, req.Op)
	if err != nil {
		out.Err = ClassifyError(err)
		e.record(actor, "kv.mutate", string(req.Op.Kind), out)
		return out
	}
	out.Result = res
	e.record(actor, "kv.mutate", string(req.Op.Kind), out)
	return out
}

// record 写审计。字段与 SQL 路径保持一致，便于同一个界面里混排。
func (e *KVExecutor) record(actor, action, target string, out *KVOutcome) {
	if e.audit == nil {
		return
	}
	outcome := "ok"
	switch {
	case out.Blocked && out.ApprovalRequired:
		outcome = "denied"
	case out.Blocked:
		outcome = "denied"
	case out.Err != nil:
		outcome = "error"
	}
	reason := out.Reason
	if reason == "" && out.Err != nil {
		reason = out.Err.Message
	}
	e.audit.Record(AuditEvent{
		ID:       newID(),
		At:       time.Now().UTC(),
		Actor:    actorOf(actor),
		Action:   action,
		Target:   target,
		Risk:     out.Risk,
		Friction: out.Friction,
		Outcome:  outcome,
		Reason:   reason,
	})
}
