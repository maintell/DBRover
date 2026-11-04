package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
	"github.com/maintell/DBRover/internal/service"
)

// inlineProvider 是包内测试用的最小 Provider。
type inlineProvider struct{}

func (inlineProvider) Name() string       { return "inline" }
func (inlineProvider) Caps() ProviderCaps { return ProviderCaps{ToolCalling: true} }
func (inlineProvider) Complete(context.Context, Request) (Response, error) {
	return Response{Text: "ok"}, nil
}

func newInnerRuntime(t *testing.T, conn driver.Connector, mode Mode) *Runtime {
	t.Helper()
	rt, err := NewRuntime(Options{
		Provider:    inlineProvider{},
		Executor:    service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil),
		Catalog:     service.NewCatalog(conn),
		Mode:        mode,
		Environment: model.EnvTest,
		AccessMode:  model.AccessReadWrite,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return rt
}

// TestChangeApplyGateHoldsUntilHumanApproves 是这套设计的关键断言：
// 高危预案在批准前一律不执行，批准之后同一张预案真的落地。
//
// 用「把预案风险上调到 R2」来构造场景：正常行级变更都是 R1，走不到审批分支。
func TestChangeApplyGateHoldsUntilHumanApproves(t *testing.T) {
	ctx := context.Background()
	conn := openInnerConn(t,
		`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO t (id, v) VALUES (1, 'before')`,
	)
	rt := newInnerRuntime(t, conn, ModeWriteWithApproval)

	env := rt.tools["change.plan"].Handle(ctx, json.RawMessage(
		`{"table":{"name":"t"},"changes":[{"op":"update","key":{"id":1},"after":{"v":"after"}}]}`))
	if !env.OK {
		t.Fatalf("生成预案失败: %+v", env.Error)
	}

	rt.mu.Lock()
	if len(rt.plans) != 1 {
		rt.mu.Unlock()
		t.Fatalf("应恰好保管一张预案，实际 %d", len(rt.plans))
	}
	var planID string
	for id, p := range rt.plans {
		planID = id
		p.Risk = policy.R2 // 构造「需要审批」的场景
	}
	rt.mu.Unlock()

	out := rt.tools["change.apply"].Handle(ctx, json.RawMessage(`{"plan_id":"`+planID+`"}`))
	if out.OK {
		t.Fatal("未经批准的高危预案不该执行")
	}
	if out.Error == nil || out.Error.Code != "APPROVAL_REQUIRED" {
		t.Fatalf("错误码 = %+v，期望 APPROVAL_REQUIRED", out.Error)
	}
	if got := innerValue(t, conn, 1); got != "before" {
		t.Fatalf("未批准时数据不该变，实际 %q", got)
	}

	if err := rt.Approve(planID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	out = rt.tools["change.apply"].Handle(ctx, json.RawMessage(`{"plan_id":"`+planID+`"}`))
	if !out.OK {
		t.Fatalf("批准后应执行成功: %+v", out.Error)
	}
	if got := innerValue(t, conn, 1); got != "after" {
		t.Fatalf("批准后数据应更新，实际 %q", got)
	}
}

// TestApproveRejectsUnknownPlan 批准一个不存在的预案必须报错，
// 而不是「先记下、等它出现再说」——那样会变成隐式放行。
func TestApproveRejectsUnknownPlan(t *testing.T) {
	conn := openInnerConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	rt := newInnerRuntime(t, conn, ModeWriteWithApproval)
	if err := rt.Approve("不存在"); err == nil {
		t.Fatal("批准不存在的预案应报错")
	}
	if len(rt.PendingPlans()) != 0 {
		t.Error("没有待批预案时清单应为空")
	}
}

// TestNoApproveTool 锁住「Agent 自己不能批准」：工具面里没有任何批准入口。
func TestNoApproveTool(t *testing.T) {
	conn := openInnerConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	rt := newInnerRuntime(t, conn, ModeWriteWithApproval)
	for name := range rt.tools {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "approve") || strings.Contains(lower, "permission") {
			t.Errorf("工具面里不该出现 %q：批准与改权限都是人类动作", name)
		}
	}
	if got := len(rt.tools); got != 14 {
		t.Errorf("工具数 = %d，期望 14（docs/11 §3.2）", got)
	}
}

func openInnerConn(t *testing.T, ddl ...string) driver.Connector {
	t.Helper()
	ctx := context.Background()
	c := sqlite.New()
	if err := c.Open(ctx, driver.ConnectConfig{
		Host:        filepath.Join(t.TempDir(), "inner.db"),
		AccessMode:  model.AccessReadWrite,
		Environment: model.EnvTest,
	}); err != nil {
		t.Fatalf("打开 sqlite: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	for _, q := range ddl {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("执行 %s: %v", q, err)
		}
	}
	return c
}

func innerValue(t *testing.T, conn driver.Connector, id int64) string {
	t.Helper()
	ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
	out, err := ex.ReadOnlyQuery(context.Background(),
		service.Request{SQL: "SELECT v FROM t WHERE id = 1", Args: []any{id}})
	if err != nil {
		t.Fatalf("复核查询: %v", err)
	}
	if len(out.Results) == 0 || len(out.Results[0].Rows) == 0 {
		return ""
	}
	v, _ := out.Results[0].Rows[0][0].(string)
	return v
}

// TestToolSchemasAreValidJSON 是一条便宜但必要的门禁：
// schema 是拼出来的字符串，坏一个字符就会让整次模型调用失败，
// 而且错误信息出现在运行时（invalid character ... after top-level value），
// 完全不指向是哪个工具。
func TestToolSchemasAreValidJSON(t *testing.T) {
	conn := openInnerConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	rt := newInnerRuntime(t, conn, ModeReadOnly)
	for _, s := range rt.Tools() {
		if !json.Valid(s.Parameters) {
			t.Errorf("工具 %s 的 schema 不是合法 JSON：%s", s.Name, s.Parameters)
		}
		if s.Name == "" || s.Description == "" {
			t.Errorf("工具 %q 缺名称或说明", s.Name)
		}
	}
}

// TestCompactNowOnlyWhenOverBudget 钉住手动压缩的两条口径。
//
// 「压了多少」这种事最容易自证：只断言 After < Before，把压缩实现整个删掉
// 也可能因为别的原因变短。所以这里同时断言历史里真的留下了「已压缩」的说明
// （模型不能以为自己看过全文），以及已经低于预算时再点一次必须什么都不做——
// 压缩有损，用户随手点两下不该反复丢细节。
func TestCompactNowOnlyWhenOverBudget(t *testing.T) {
	conn := openInnerConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	rt := newInnerRuntime(t, conn, ModeReadOnly)
	rt.contextBudget = 400
	rt.history = []Message{
		{Role: "user", Content: "问一句"},
		{Role: "tool", Name: "query.execute", Content: strings.Repeat("x", 800)},
	}

	res := rt.CompactNow()
	if res.Skipped || res.Compacted != 1 {
		t.Fatalf("超预算时应压缩 1 条：%+v", res)
	}
	if res.After >= res.Before {
		t.Errorf("压缩后体积没变小：%d → %d", res.Before, res.After)
	}
	found := false
	for _, m := range rt.history {
		if strings.Contains(m.Content, "已压缩") {
			found = true
		}
	}
	if !found {
		t.Error("压缩后必须留下「已压缩」的说明，否则模型会以为看过全文")
	}

	res2 := rt.CompactNow()
	if !res2.Skipped {
		t.Fatalf("已经低于预算时再点一次应什么都不做：%+v", res2)
	}
	if res2.Before != res2.After {
		t.Errorf("跳过时不该改体积：%d → %d", res2.Before, res2.After)
	}
}
