package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/agent"
	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// newConn 建一个装了测试表的 SQLite 连接。
func newConn(t *testing.T, ddl ...string) driver.Connector {
	t.Helper()
	ctx := context.Background()
	c := sqlite.New()
	if err := c.Open(ctx, driver.ConnectConfig{
		Host:        filepath.Join(t.TempDir(), "agent.db"),
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

// scripted 是一个按脚本回答的 Provider：每一步都由测试给。
//
// 用它而不是「真的调一次模型」，是因为要验证的是循环与权限，不是模型的智力。
// 脚本可以直接读上一轮的工具结果，因此能模拟「模型看到 plan_id 后再调 apply」。
type scripted struct {
	// noTools 让 Caps 声明「不支持工具调用」，用于验证拒绝路径。
	noTools bool
	steps   []func(req agent.Request) (agent.Response, error)
	n       int
}

func (p *scripted) Name() string { return "scripted" }
func (p *scripted) Caps() agent.ProviderCaps {
	return agent.ProviderCaps{ToolCalling: !p.noTools}
}

func (p *scripted) Complete(_ context.Context, req agent.Request) (agent.Response, error) {
	if p.n >= len(p.steps) {
		return agent.Response{Text: "（脚本用完了）"}, nil
	}
	fn := p.steps[p.n]
	p.n++
	return fn(req)
}

// say 返回一条纯文本答复。
func say(text string) func(agent.Request) (agent.Response, error) {
	return func(agent.Request) (agent.Response, error) { return agent.Response{Text: text}, nil }
}

// callTool 返回一次工具调用。
func callTool(name, args string) func(agent.Request) (agent.Response, error) {
	return func(agent.Request) (agent.Response, error) {
		return agent.Response{ToolCalls: []agent.ToolCall{{
			ID: "call-1", Name: name, Arguments: json.RawMessage(args),
		}}}, nil
	}
}

// lastToolResult 取最后一条工具消息的内容，用于「模型根据结果决定下一步」。
func lastToolResult(req agent.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "tool" {
			return req.Messages[i].Content
		}
	}
	return ""
}

func newRuntime(t *testing.T, prov agent.Provider, conn driver.Connector, m agent.Mode) *agent.Runtime {
	t.Helper()
	rt, err := agent.NewRuntime(agent.Options{
		Provider:    prov,
		Executor:    service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil),
		Catalog:     service.NewCatalog(conn),
		Mode:        m,
		Environment: model.EnvTest,
		AccessMode:  model.AccessReadWrite,
		MaxSteps:    8,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return rt
}

// TestNewRuntimeRejectsModelWithoutToolCalling 锁住 docs/11 §8.2 那条硬要求：
// 不支持工具调用的模型不许进 Agent 模式。
func TestNewRuntimeRejectsModelWithoutToolCalling(t *testing.T) {
	conn := newConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	prov := &scripted{noTools: true}
	_, err := agent.NewRuntime(agent.Options{
		Provider: prov,
		Executor: service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil),
		Catalog:  service.NewCatalog(conn),
		Mode:     agent.ModeReadOnly,
	})
	if err != agent.ErrNoToolCalling {
		t.Fatalf("期望 ErrNoToolCalling，实际 %v", err)
	}
}

// TestAskWithoutTools 模型直接给结论时不产生任何步骤。
func TestAskWithoutTools(t *testing.T) {
	conn := newConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	prov := &scripted{steps: []func(agent.Request) (agent.Response, error){say("直接回答")}}
	rt := newRuntime(t, prov, conn, agent.ModeReadOnly)

	res, err := rt.Ask(context.Background(), "你好")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Answer != "直接回答" {
		t.Errorf("Answer = %q", res.Answer)
	}
	if len(res.Steps) != 0 {
		t.Errorf("不该有步骤: %+v", res.Steps)
	}
}

// TestAskRunsToolAndFeedsBack 工具真的被执行，结果真的回到了模型手里。
func TestAskRunsToolAndFeedsBack(t *testing.T) {
	conn := newConn(t,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, amount INTEGER)`,
		`INSERT INTO orders (id, amount) VALUES (1, 10), (2, 20)`,
	)
	prov := &scripted{steps: []func(agent.Request) (agent.Response, error){
		callTool("query.execute", `{"sql":"SELECT COUNT(*) AS n FROM orders"}`),
		func(req agent.Request) (agent.Response, error) {
			got := lastToolResult(req)
			if !strings.Contains(got, `"ok":true`) || !strings.Contains(got, `"rows":[[2]]`) {
				return agent.Response{}, errBad("工具结果没有回到模型：" + got)
			}
			return agent.Response{Text: "表里有 2 行"}, nil
		},
	}}
	rt := newRuntime(t, prov, conn, agent.ModeReadOnly)

	res, err := rt.Ask(context.Background(), "orders 有多少行")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(res.Steps) != 1 || !res.Steps[0].OK {
		t.Fatalf("应有一次成功的工具调用: %+v", res.Steps)
	}
	if res.Answer != "表里有 2 行" {
		t.Errorf("Answer = %q", res.Answer)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
func errBad(s string) error       { return errString(s) }

// TestQueryExecuteRefusesWrites 关键安全断言：只读工具不能成为后门。
// 分类在 service 侧做，Agent 只能收到一个明确的拒绝。
func TestQueryExecuteRefusesWrites(t *testing.T) {
	conn := newConn(t,
		`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO t (id, v) VALUES (1, 'a')`,
	)
	prov := &scripted{steps: []func(agent.Request) (agent.Response, error){
		callTool("query.execute", `{"sql":"DELETE FROM t"}`),
		say("写不了"),
	}}
	rt := newRuntime(t, prov, conn, agent.ModeWriteWithApproval)
	res, err := rt.Ask(context.Background(), "删掉 t")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("步骤数 = %d", len(res.Steps))
	}
	if res.Steps[0].OK {
		t.Fatal("DELETE 不该被当成只读查询执行")
	}
	if res.Steps[0].Error == nil || res.Steps[0].Error.Code != "READ_ONLY_REQUIRED" {
		t.Fatalf("错误码 = %+v", res.Steps[0].Error)
	}

	// 数据必须还在。
	ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
	out, err := ex.ReadOnlyQuery(context.Background(), service.Request{SQL: "SELECT COUNT(*) AS n FROM t"})
	if err != nil {
		t.Fatalf("复核查询失败: %v", err)
	}
	if len(out.Results) == 0 || len(out.Results[0].Rows) != 1 {
		t.Fatalf("复核结果异常: %+v", out.Results)
	}
	if got := out.Results[0].Rows[0][0]; got == nil || got.(int64) != 1 {
		t.Fatalf("行数 = %v，期望 1（DELETE 不该生效）", got)
	}
}

// TestReadOnlyModeDeniesWriteTool 只读模式下写类工具被授权层直接拦下。
func TestReadOnlyModeDeniesWriteTool(t *testing.T) {
	conn := newConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	prov := &scripted{steps: []func(agent.Request) (agent.Response, error){
		callTool("change.plan", `{"table":{"name":"t"},"changes":[{"op":"update","key":{"id":1},"after":{"v":"x"}}]}`),
		say("好的"),
	}}
	rt := newRuntime(t, prov, conn, agent.ModeReadOnly)
	res, err := rt.Ask(context.Background(), "改一行")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !res.Steps[0].Denied {
		t.Fatalf("只读模式下 change.plan 应被拒: %+v", res.Steps[0])
	}
	if res.Steps[0].Error == nil || res.Steps[0].Error.Code != "DENIED" {
		t.Fatalf("错误码 = %+v", res.Steps[0].Error)
	}
}

// TestLoopDetected 同工具同参数连着调两次即中断，不再白跑。
func TestLoopDetected(t *testing.T) {
	conn := newConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	prov := &scripted{}
	prov.steps = []func(agent.Request) (agent.Response, error){}
	loop := callTool("meta.objects", `{}`)
	prov.steps = append(prov.steps, loop, loop, loop, loop)
	rt := newRuntime(t, prov, conn, agent.ModeReadOnly)

	res, err := rt.Ask(context.Background(), "看看有哪些表")
	if err != agent.ErrLoopDetected {
		t.Fatalf("期望 ErrLoopDetected，实际 %v", err)
	}
	if res.Stopped != "loop" {
		t.Errorf("Stopped = %q", res.Stopped)
	}
}

// TestStepBudget 用不同的参数反复调同一个工具，不会触发循环检测，
// 但会在步数上限处停下，并且把「为什么停」说清楚。
func TestStepBudget(t *testing.T) {
	conn := newConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	prov := &scripted{}
	for i := 0; i < 20; i++ {
		prov.steps = append(prov.steps, callTool("note.write",
			`{"text":"`+string(rune('a'+i))+`"}`))
	}
	rt, err := agent.NewRuntime(agent.Options{
		Provider: prov,
		Executor: service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil),
		Catalog:  service.NewCatalog(conn),
		Mode:     agent.ModeReadOnly,
		MaxSteps: 3,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	res, err := rt.Ask(context.Background(), "记点什么")
	if err != agent.ErrStepBudget {
		t.Fatalf("期望 ErrStepBudget，实际 %v", err)
	}
	if res.Stopped != "step_budget" {
		t.Errorf("Stopped = %q", res.Stopped)
	}
	if len(res.Steps) != 3 {
		t.Errorf("步数 = %d，期望 3", len(res.Steps))
	}
	if len(res.Notes) != 3 {
		t.Errorf("笔记 = %v", res.Notes)
	}
}

// TestToolFailureCap 同一个工具连续失败到上限就停，不无限重试。
func TestToolFailureCap(t *testing.T) {
	conn := newConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	prov := &scripted{}
	for i := 0; i < 10; i++ {
		prov.steps = append(prov.steps, callTool("meta.describe",
			`{"name":"不存在的表_`+string(rune('a'+i))+`"}`))
	}
	rt, err := agent.NewRuntime(agent.Options{
		Provider: prov,
		Executor: service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil),
		Catalog:  service.NewCatalog(conn),
		Mode:     agent.ModeReadOnly,
		MaxSteps: 20,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	res, err := rt.Ask(context.Background(), "描述一张不存在的表")
	if err != agent.ErrToolFailures {
		t.Fatalf("期望 ErrToolFailures，实际 %v", err)
	}
	if res.Stopped != "tool_failures" {
		t.Errorf("Stopped = %q", res.Stopped)
	}
	if len(res.Steps) > 4 {
		t.Errorf("失败上限 3 次，不该跑这么多步: %d", len(res.Steps))
	}
}

// TestConnInfoAndObjects 冒烟两个最常用的 R0 工具。
func TestConnInfoAndObjects(t *testing.T) {
	conn := newConn(t,
		`CREATE TABLE alpha (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE beta (id INTEGER PRIMARY KEY)`,
	)
	prov := &scripted{steps: []func(agent.Request) (agent.Response, error){
		callTool("conn.info", `{}`),
		callTool("meta.objects", `{"kinds":["table"]}`),
		func(req agent.Request) (agent.Response, error) {
			got := lastToolResult(req)
			if !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") {
				return agent.Response{}, errBad("对象清单不完整：" + got)
			}
			return agent.Response{Text: "有两张表"}, nil
		},
	}}
	rt := newRuntime(t, prov, conn, agent.ModeReadOnly)
	res, err := rt.Ask(context.Background(), "有哪些表")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	for i, s := range res.Steps {
		if !s.OK {
			t.Errorf("第 %d 步失败: %+v", i, s)
		}
	}
}

// TestArgsAreStrict 多余字段直接拒绝，而不是被忽略。
func TestArgsAreStrict(t *testing.T) {
	conn := newConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	prov := &scripted{steps: []func(agent.Request) (agent.Response, error){
		callTool("meta.objects", `{"limit":10,"nonsense":true}`),
		say("知道了"),
	}}
	rt := newRuntime(t, prov, conn, agent.ModeReadOnly)
	res, err := rt.Ask(context.Background(), "列表")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Steps[0].OK {
		t.Fatal("带多余字段的参数不该被接受")
	}
	if res.Steps[0].Error == nil || res.Steps[0].Error.Code != "ARGS_INVALID" {
		t.Fatalf("错误码 = %+v", res.Steps[0].Error)
	}
}

// TestOpenAIWireFormat 用 httptest 顶替模型服务，验证协议编解码这一层：
// 请求里带上了工具定义与 system、Authorization 正确；响应里的
// tool_calls（arguments 是字符串）被正确解析。
func TestOpenAIWireFormat(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"",
				"tool_calls":[{"id":"c1","type":"function",
					"function":{"name":"meta.objects","arguments":"{\"limit\":5}"}}]},
				"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
	}))
	defer srv.Close()

	p, err := agent.NewOpenAI(agent.OpenAIConfig{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "test-model",
	})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	resp, err := p.Complete(context.Background(), agent.Request{
		System:   "你是助手",
		Messages: []agent.Message{{Role: "user", Content: "有哪些表"}},
		Tools: []agent.ToolSchema{{
			Name: "meta.objects", Description: "列出对象",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody["model"] != "test-model" {
		t.Errorf("model = %v", gotBody["model"])
	}
	if gotBody["stream"] != false {
		t.Errorf("stream = %v，期望 false", gotBody["stream"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 条数 = %d", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是助手" {
		t.Errorf("第一条消息 = %v", first)
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 条数 = %d", len(tools))
	}
	tool0, _ := tools[0].(map[string]any)
	if tool0["type"] != "function" {
		t.Errorf("tools[0].type = %v", tool0["type"])
	}
	fn, _ := tool0["function"].(map[string]any)
	if fn["name"] != "meta.objects" {
		t.Errorf("tools[0].function.name = %v", fn["name"])
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool_calls 条数 = %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "meta.objects" {
		t.Errorf("工具名 = %q", resp.ToolCalls[0].Name)
	}
	if string(resp.ToolCalls[0].Arguments) != `{"limit":5}` {
		t.Errorf("参数 = %s", resp.ToolCalls[0].Arguments)
	}
	if resp.Usage.TotalTokens != 18 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

// TestOpenAIReportsHTTPError 服务端 500 时要给出可诊断的错误，而不是空结果。
func TestOpenAIReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()

	p, err := agent.NewOpenAI(agent.OpenAIConfig{BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	_, err = p.Complete(context.Background(), agent.Request{})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("错误信息里应带状态码，实际 %v", err)
	}
}

// TestReadOnlyModeDeniesApply 对应验收 1：只读模式下执行入口被拒，
// 且模型拿到的是可解释的错误（不是一句 internal error）。
func TestReadOnlyModeDeniesApply(t *testing.T) {
	conn := newConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	prov := &scripted{steps: []func(agent.Request) (agent.Response, error){
		callTool("change.apply", `{"plan_id":"任意"}`),
		say("被拒了"),
	}}
	rt := newRuntime(t, prov, conn, agent.ModeReadOnly)
	res, err := rt.Ask(context.Background(), "执行它")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !res.Steps[0].Denied {
		t.Fatalf("只读模式应拒绝 change.apply: %+v", res.Steps[0])
	}
	if res.Steps[0].Error == nil || res.Steps[0].Error.Message == "" {
		t.Fatal("拒绝必须带可解释的原因")
	}
}

// TestNoToolTakesConnectionID 对应验收 5：工具参数里没有「连接」这个概念，
// 所以模型无法把操作指向另一条连接。
func TestNoToolTakesConnectionID(t *testing.T) {
	conn := newConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	rt := newRuntime(t, &scripted{}, conn, agent.ModeReadOnly)
	for _, s := range rt.Tools() {
		params := strings.ToLower(string(s.Parameters))
		if strings.Contains(params, "connection") || strings.Contains(params, "conn_id") {
			t.Errorf("工具 %s 的参数里出现了连接概念：%s", s.Name, s.Parameters)
		}
	}
}

// TestLargeResultIsTruncated 对应验收 8：1000 行以上只给 20 行 + 聚合画像。
func TestLargeResultIsTruncated(t *testing.T) {
	conn := newConn(t,
		`CREATE TABLE big (id INTEGER PRIMARY KEY, bucket TEXT)`,
		`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 1200)
		 INSERT INTO big (id, bucket) SELECT n, 'b' || (n % 4) FROM seq`,
	)
	prov := &scripted{steps: []func(agent.Request) (agent.Response, error){
		callTool("query.execute", `{"sql":"SELECT id, bucket FROM big","max_rows":5000}`),
		func(req agent.Request) (agent.Response, error) {
			got := lastToolResult(req)
			if !strings.Contains(got, `"rows_shown":20`) {
				return agent.Response{}, errBad("没有按 20 行截断：" + got)
			}
			if !strings.Contains(got, `"rows_total":1200`) {
				return agent.Response{}, errBad("没有报出总行数：" + got)
			}
			if !strings.Contains(got, `"stats"`) {
				return agent.Response{}, errBad("缺聚合画像：" + got)
			}
			return agent.Response{Text: "1200 行，按 bucket 分四组"}, nil
		},
	}}
	rt := newRuntime(t, prov, conn, agent.ModeReadOnly)
	if _, err := rt.Ask(context.Background(), "看看 big 表"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
}
