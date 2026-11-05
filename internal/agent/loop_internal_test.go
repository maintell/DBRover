package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/policy"
)

// scriptProvider 按剧本回放：每一步给出固定的工具调用，剧本用完就给结论。
//
// 用它而不是真模型：这几条断言考的都是「模型说了这句话之后我们这边怎么反应」，
// 与模型智力无关（docs/11 §11.1 的录制回放同一思路）。
type scriptProvider struct {
	steps []Response
	i     int
}

func (p *scriptProvider) Name() string       { return "script" }
func (p *scriptProvider) Caps() ProviderCaps { return ProviderCaps{ToolCalling: true} }

func (p *scriptProvider) Complete(context.Context, Request) (Response, error) {
	if p.i >= len(p.steps) {
		return Response{Text: "完成"}, nil
	}
	r := p.steps[p.i]
	p.i++
	return r, nil
}

func call(name, args string) Response {
	return Response{ToolCalls: []ToolCall{{
		ID: "c-" + name, Name: name, Arguments: json.RawMessage(args),
	}}}
}

// TestAlternatingLoopDetected 锁住 A、B、A、B 这种交替空转（docs/11 §7.3）。
//
// 「连续两次同工具同参数」抓不到它：每一步的签名都和上一步不同。但它同样是
// 原地打转——两步走一圈，信息量没有增加，只是把步数预算烧光。
func TestAlternatingLoopDetected(t *testing.T) {
	conn := openInnerConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	rt := newInnerRuntime(t, conn, ModeReadOnly)
	rt.prov = &scriptProvider{steps: []Response{
		call("query.execute", `{"sql":"SELECT 1 AS a"}`),
		call("query.execute", `{"sql":"SELECT 2 AS b"}`),
		call("query.execute", `{"sql":"SELECT 1 AS a"}`),
		call("query.execute", `{"sql":"SELECT 2 AS b"}`),
	}}

	res, err := rt.Ask(context.Background(), "看看数据")
	if !errors.Is(err, ErrLoopDetected) {
		t.Fatalf("期望 ErrLoopDetected，实际 %v（stopped=%q）", err, res.Stopped)
	}
	if res.Stopped != "loop" {
		t.Errorf("Stopped = %q，期望 loop", res.Stopped)
	}
	if len(res.Steps) != 4 {
		t.Errorf("应在第 4 次调用后停下，实际执行了 %d 步", len(res.Steps))
	}
}

// TestDifferentQueriesAreNotLoop 是反向断言：真的在换着查不该被误判成循环。
//
// 少了这条，「检测到循环」可以靠「一有重复就停」实现，而那会打断正常工作。
func TestDifferentQueriesAreNotLoop(t *testing.T) {
	conn := openInnerConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	rt := newInnerRuntime(t, conn, ModeReadOnly)
	rt.prov = &scriptProvider{steps: []Response{
		call("query.execute", `{"sql":"SELECT 1 AS a"}`),
		call("query.execute", `{"sql":"SELECT 2 AS b"}`),
		call("query.execute", `{"sql":"SELECT 3 AS c"}`),
		call("query.execute", `{"sql":"SELECT 4 AS d"}`),
	}}

	res, err := rt.Ask(context.Background(), "看看数据")
	if err != nil {
		t.Fatalf("四条不同的查询不该被判成循环：%v（stopped=%q）", err, res.Stopped)
	}
	if res.Answer == "" {
		t.Error("应给出结论")
	}
}

// TestApprovalPendingStopsRun 锁住「未批准的 apply 就地挂起」（docs/11 §5.2）。
//
// 不挂起的话，模型会反复重试同一个 apply，第 3 次撞上失败上限，用户看到的是
// 「工具反复失败」——真实原因却是「还没人签字」。这两种结论对用户的价值完全不同。
func TestApprovalPendingStopsRun(t *testing.T) {
	ctx := context.Background()
	conn := openInnerConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO t (id, v) VALUES (1, 'before')`)
	rt := newInnerRuntime(t, conn, ModeWriteWithApproval)

	env := rt.tools["change.plan"].Handle(ctx, json.RawMessage(
		`{"table":{"name":"t"},"changes":[{"op":"update","key":{"id":1},"after":{"v":"after"}}]}`))
	if !env.OK {
		t.Fatalf("生成预案失败: %+v", env.Error)
	}
	var planID string
	rt.mu.Lock()
	for id, p := range rt.plans {
		planID = id
		p.Risk = policy.R2 // 构造「需要审批」的场景
	}
	rt.mu.Unlock()

	rt.prov = &scriptProvider{steps: []Response{
		call("change.apply", `{"plan_id":"`+planID+`"}`),
		// 若挂起没生效，模型会走到这一步并成功执行——下面用它来判失败。
		call("change.apply", `{"plan_id":"`+planID+`"}`),
	}}

	var approvals int
	res, err := rt.AskStream(ctx, "把 v 改成 after", func(ev Event) {
		if ev.Kind == "approval_request" {
			approvals++
			if ev.Plan == nil || ev.Plan.ID != planID {
				t.Errorf("审批事件里的预案不对：%+v", ev.Plan)
			}
		}
	})
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("期望 ErrApprovalPending，实际 %v（stopped=%q）", err, res.Stopped)
	}
	if res.Stopped != "approval" {
		t.Errorf("Stopped = %q，期望 approval", res.Stopped)
	}
	if approvals != 1 {
		t.Errorf("应推送 1 次审批请求，实际 %d 次", approvals)
	}
	if len(res.Steps) != 1 {
		t.Errorf("挂起后不该继续调用工具，实际 %d 步", len(res.Steps))
	}
	if !strings.Contains(res.Answer, planID) {
		t.Errorf("结论里应说明停在哪个预案上，实际 %q", res.Answer)
	}
	if got := innerValue(t, conn, 1); got != "before" {
		t.Errorf("未批准时数据不该变，实际 %q", got)
	}
	if len(rt.PendingPlans()) != 1 {
		t.Errorf("预案应留在待批清单里，实际 %d 张", len(rt.PendingPlans()))
	}
}

// TestStepSampleCarriesResultButExportDropsIt 锁住两条同时成立的要求：
//   - 界面要能「展开看真实结果」（docs/11 §9 必须做到 1）；
//   - 会话导出不带业务数据（docs/11 §13 第 13 条）。
func TestStepSampleCarriesResultButExportDropsIt(t *testing.T) {
	conn := openInnerConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO t (id, v) VALUES (1, 'x')`)
	rt := newInnerRuntime(t, conn, ModeReadOnly)
	rt.prov = &scriptProvider{steps: []Response{
		call("query.execute", `{"sql":"SELECT v FROM t"}`),
	}}

	res, err := rt.Ask(context.Background(), "看看 t")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("期望 1 步，实际 %d", len(res.Steps))
	}
	if len(res.Steps[0].Sample) == 0 {
		t.Fatal("步骤应带工具返回的真实结果（界面要能展开看）")
	}
	if !strings.Contains(string(res.Steps[0].Sample), `"rows"`) {
		t.Errorf("样本里应含结果行，实际 %s", res.Steps[0].Sample)
	}

	tr := rt.Export()
	if len(tr.Turns) != 1 || len(tr.Turns[0].Steps) != 1 {
		t.Fatalf("导出应留下这一轮：%+v", tr.Turns)
	}
	if got := tr.Turns[0].Steps[0].Sample; len(got) != 0 {
		t.Errorf("导出不该带步骤样本（会复制业务数据），实际 %s", got)
	}
}
