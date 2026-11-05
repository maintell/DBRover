package api_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/api"
)

// fakeModel 是一个 OpenAI 兼容的假模型服务。
//
// 用它的理由和 agent 包里的脚本化 Provider 一样：要验的是协议与权限，
// 不是模型的智力。每次被调用时按 call 序号返回脚本里的响应。
type fakeModel struct {
	calls  int
	script []string
}

func (f *fakeModel) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("假模型收到非法请求体: %v", err)
		}
		n := f.calls
		f.calls++
		var content string
		if n < len(f.script) {
			content = f.script[n]
		} else {
			content = textReply("（脚本用完了）")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":%s,"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, content)
	}
}

// textReply 构造一条纯文本答复。
func textReply(text string) string {
	b, _ := json.Marshal(text)
	return `{"role":"assistant","content":` + string(b) + `}`
}

// toolReply 构造一条工具调用答复。args 是 JSON 原文。
func toolReply(id, name, args string) string {
	b, _ := json.Marshal(args)
	return `{"role":"assistant","content":"","tool_calls":[{"id":"` + id +
		`","type":"function","function":{"name":"` + name + `","arguments":` + string(b) + `}}]}`
}

// execSQL 执行一条写语句，遇到审批要求时用返回的 token 重提。
func execSQL(t *testing.T, ts *httptest.Server, connID, sql string) {
	t.Helper()
	code, body := do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/exec",
		map[string]any{"sql": sql}, true)
	if code == http.StatusPreconditionRequired {
		er, _ := body["error"].(map[string]any)
		ap, _ := er["approval"].(map[string]any)
		token, _ := ap["token"].(string)
		code, body = do(t, ts, http.MethodPost, "/api/v1/connections/"+connID+"/exec",
			map[string]any{"sql": sql, "approval_token": token}, true)
	}
	if code >= 300 {
		t.Fatalf("执行 %q 失败: %d %v", sql, code, body)
	}
}

var planIDPattern = regexp.MustCompile(`\\?"plan_id\\?":\\?"([^"\\]+)`)

// lastPlanID 从请求体里抠出上一个工具结果中的预案 ID。
// 模型看到的就是这段 JSON，这里模拟「模型读到了 plan_id」。
func lastPlanID(raw []byte) string {
	m := planIDPattern.FindSubmatch(raw)
	if len(m) < 2 {
		return ""
	}
	return string(m[1])
}

// TestAgentStatusAndConfigGate 未配置模型服务时，端点要明确说清楚缺什么。
func TestAgentStatusAndConfigGate(t *testing.T) {
	_, ts, dir := newServer(t)

	code, body := do(t, ts, http.MethodGet, "/api/v1/agent/status", nil, true)
	if code != http.StatusOK {
		t.Fatalf("status 状态码 = %d", code)
	}
	if body["configured"] != false {
		t.Fatalf("未配置时 configured 应为 false: %v", body)
	}
	if s, _ := body["hint"].(string); s == "" {
		t.Error("未配置时必须给出 hint")
	}

	connID := createConn(t, ts, dir, "test", "read-write")
	code, body = do(t, ts, http.MethodPost, "/api/v1/agent/conversations",
		map[string]any{"connection_id": connID, "mode": "read-only"}, true)
	if code != http.StatusBadRequest {
		t.Fatalf("未配置时新建会话应 400，实际 %d: %v", code, body)
	}
	er, _ := body["error"].(map[string]any)
	if er["code"] != "AGENT_NOT_CONFIGURED" {
		t.Fatalf("错误码 = %v", er["code"])
	}
}

// TestAgentConversationRunsQuery 走一遍真实链路：HTTP → Runtime → 工具 → SQLite。
func TestAgentConversationRunsQuery(t *testing.T) {
	s, ts, dir := newServer(t)
	fake := &fakeModel{script: []string{
		toolReply("c1", "query.execute", `{"sql":"SELECT COUNT(*) AS n FROM items"}`),
		textReply("表里没有数据"),
	}}
	model := httptest.NewServer(fake.handler(t))
	defer model.Close()
	s.SetAgentConfig(api.AgentConfig{BaseURL: model.URL, Model: "fake"})

	connID := createConn(t, ts, dir, "test", "read-write")
	execSQL(t, ts, connID, "CREATE TABLE items (id INTEGER PRIMARY KEY)")

	code, body := do(t, ts, http.MethodPost, "/api/v1/agent/conversations",
		map[string]any{"connection_id": connID, "mode": "read-only"}, true)
	if code != http.StatusCreated {
		t.Fatalf("新建会话 = %d %v", code, body)
	}
	convID, _ := body["id"].(string)
	if convID == "" {
		t.Fatal("没有返回会话 id")
	}
	if body["mode"] != "read-only" {
		t.Errorf("mode = %v", body["mode"])
	}
	if n, _ := body["tools"].(float64); n != 14 {
		t.Errorf("工具数 = %v，期望 14", body["tools"])
	}

	code, body = do(t, ts, http.MethodPost, "/api/v1/agent/conversations/"+convID+"/messages",
		map[string]any{"content": "items 有多少行"}, true)
	if code != http.StatusOK {
		t.Fatalf("发消息 = %d %v", code, body)
	}
	if body["answer"] != "表里没有数据" {
		t.Errorf("answer = %v", body["answer"])
	}
	steps, _ := body["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("步骤数 = %d", len(steps))
	}
	step0, _ := steps[0].(map[string]any)
	if step0["tool"] != "query.execute" || step0["ok"] != true {
		t.Errorf("第一步 = %v", step0)
	}
	if body["stopped"] != "" {
		t.Errorf("不该有停止原因: %v", body["stopped"])
	}
}

// TestAgentApprovalEndpoint 走一遍审批：预案生成 → 被拦下 → 人批准 → 执行成功。
func TestAgentApprovalEndpoint(t *testing.T) {
	s, ts, dir := newServer(t)
	fake := &fakeModel{script: []string{
		toolReply("c1", "change.plan", `{"ddl":{"table":{"name":"items"},
			"action":"create_index","index":{"name":"idx_v","columns":["v"]}}}`),
		// 第 2 次：模型读到 plan_id 后要求执行（此时还没批准）——这一轮会就地挂起，
		// 所以脚本里不再有「等你确认」那句话：结论由我们写，不替模型发言。
		"__apply__",
		// 第 3 次：人批准之后，模型再要求执行一次
		"__apply__",
		textReply("索引已建好"),
	}}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				body = append(body, buf[:n]...)
			}
			if err != nil {
				break
			}
		}
		n := fake.calls
		fake.calls++
		var content string
		switch {
		case n >= len(fake.script):
			content = textReply("（脚本用完了）")
		case fake.script[n] == "__apply__":
			id := lastPlanID(body)
			if id == "" {
				t.Errorf("第 %d 次调用：请求体里没有 plan_id", n)
			}
			content = toolReply("c-apply", "change.apply", `{"plan_id":"`+id+`"}`)
		default:
			content = fake.script[n]
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":%s,"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, content)
	}))
	defer model.Close()
	s.SetAgentConfig(api.AgentConfig{BaseURL: model.URL, Model: "fake"})

	connID := createConn(t, ts, dir, "test", "read-write")
	execSQL(t, ts, connID, "CREATE TABLE items (id INTEGER PRIMARY KEY, v TEXT)")

	code, body := do(t, ts, http.MethodPost, "/api/v1/agent/conversations",
		map[string]any{"connection_id": connID, "mode": "write-with-approval"}, true)
	if code != http.StatusCreated {
		t.Fatalf("新建会话 = %d %v", code, body)
	}
	convID, _ := body["id"].(string)

	// 第一轮：生成 R3 预案并尝试执行 → 被拦下，并带回待批预案。
	code, body = do(t, ts, http.MethodPost, "/api/v1/agent/conversations/"+convID+"/messages",
		map[string]any{"content": "给 v 建个索引"}, true)
	if code != http.StatusOK {
		t.Fatalf("发消息 = %d %v", code, body)
	}
	steps, _ := body["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("步骤数 = %d，期望 2", len(steps))
	}
	applyStep, _ := steps[1].(map[string]any)
	if applyStep["ok"] == true {
		t.Fatal("未经批准的 R3 预案不该执行")
	}
	er, _ := applyStep["error"].(map[string]any)
	if er["code"] != "APPROVAL_REQUIRED" {
		t.Fatalf("错误码 = %v", er["code"])
	}
	// 未批准 = 这一轮挂起，而不是「跑完了但没执行」：界面据此知道要等人签字。
	if body["stopped"] != "approval" {
		t.Errorf("stopped = %v，期望 approval", body["stopped"])
	}
	pending, _ := body["pending_approvals"].([]any)
	if len(pending) != 1 {
		t.Fatalf("待批预案数 = %d", len(pending))
	}
	card, _ := pending[0].(map[string]any)
	planID, _ := card["plan_id"].(string)
	if planID == "" {
		t.Fatal("审批卡片里没有 plan_id")
	}
	// 审批卡片必须带最终 SQL，而不是一句描述。
	if stmts, _ := card["statements"].([]any); len(stmts) == 0 {
		t.Fatal("审批卡片必须带最终 SQL")
	}

	// 人批准。
	code, body = do(t, ts, http.MethodPost,
		"/api/v1/agent/conversations/"+convID+"/approvals/"+planID,
		map[string]any{"approve": true}, true)
	if code != http.StatusOK || body["approved"] != true {
		t.Fatalf("批准 = %d %v", code, body)
	}

	// 第二轮：模型再执行一次 → 这次应该成功。
	code, body = do(t, ts, http.MethodPost, "/api/v1/agent/conversations/"+convID+"/messages",
		map[string]any{"content": "现在执行"}, true)
	if code != http.StatusOK {
		t.Fatalf("第二轮 = %d %v", code, body)
	}
	steps, _ = body["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("第二轮步骤数 = %d", len(steps))
	}
	done, _ := steps[0].(map[string]any)
	if done["ok"] != true {
		t.Fatalf("批准后应执行成功: %v", done)
	}
	if n, _ := body["pending_approvals"].([]any); len(n) != 0 {
		t.Errorf("执行完不该还有待批预案: %v", n)
	}
}

// TestAgentApprovalRejectsUnknownPlan 批准不存在的预案要 404，不能默默放行。
func TestAgentApprovalRejectsUnknownPlan(t *testing.T) {
	s, ts, dir := newServer(t)
	fake := &fakeModel{}
	model := httptest.NewServer(fake.handler(t))
	defer model.Close()
	s.SetAgentConfig(api.AgentConfig{BaseURL: model.URL, Model: "fake"})

	connID := createConn(t, ts, dir, "test", "read-write")
	_, body := do(t, ts, http.MethodPost, "/api/v1/agent/conversations",
		map[string]any{"connection_id": connID, "mode": "read-only"}, true)
	convID, _ := body["id"].(string)

	code, body := do(t, ts, http.MethodPost,
		"/api/v1/agent/conversations/"+convID+"/approvals/不存在", map[string]any{}, true)
	if code != http.StatusNotFound {
		t.Fatalf("状态码 = %d %v", code, body)
	}
}

// TestAgentStreamIsNDJSON 流式：一条一行，步骤边跑边出，最后给结果与待批预案。
func TestAgentStreamIsNDJSON(t *testing.T) {
	s, ts, dir := newServer(t)
	fake := &fakeModel{script: []string{
		toolReply("c1", "meta.objects", `{}`),
		textReply("只有一张表"),
	}}
	model := httptest.NewServer(fake.handler(t))
	defer model.Close()
	s.SetAgentConfig(api.AgentConfig{BaseURL: model.URL, Model: "fake"})

	connID := createConn(t, ts, dir, "test", "read-write")
	execSQL(t, ts, connID, "CREATE TABLE items (id INTEGER PRIMARY KEY)")
	_, body := do(t, ts, http.MethodPost, "/api/v1/agent/conversations",
		map[string]any{"connection_id": connID, "mode": "read-only"}, true)
	convID, _ := body["id"].(string)

	req, err := http.NewRequest(http.MethodPost,
		ts.URL+"/api/v1/agent/conversations/"+convID+"/messages?stream=1",
		strings.NewReader(`{"content":"有哪些表"}`))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("X-DBRover-Token", testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "ndjson") {
		t.Errorf("Content-Type = %q，期望 ndjson", ct)
	}

	var kinds []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("不是合法 JSON 行: %q", sc.Text())
		}
		k, _ := line["kind"].(string)
		kinds = append(kinds, k)
	}
	want := []string{"step", "answer", "done", "result"}
	if len(kinds) != len(want) {
		t.Fatalf("行序列 = %v，期望 %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("行序列 = %v，期望 %v", kinds, want)
		}
	}
}

// TestAgentExport 导出会话：每轮的步骤与结论都在，且不含工具结果全文。
func TestAgentExport(t *testing.T) {
	s, ts, dir := newServer(t)
	fake := &fakeModel{script: []string{
		textReply("没什么可说的"),
	}}
	model := httptest.NewServer(fake.handler(t))
	defer model.Close()
	s.SetAgentConfig(api.AgentConfig{BaseURL: model.URL, Model: "fake"})

	connID := createConn(t, ts, dir, "test", "read-write")
	_, body := do(t, ts, http.MethodPost, "/api/v1/agent/conversations",
		map[string]any{"connection_id": connID, "mode": "read-only"}, true)
	convID, _ := body["id"].(string)

	if code, b := do(t, ts, http.MethodPost,
		"/api/v1/agent/conversations/"+convID+"/messages",
		map[string]any{"content": "说点什么"}, true); code != http.StatusOK {
		t.Fatalf("发消息失败: %d %v", code, b)
	}

	code, body := do(t, ts, http.MethodGet,
		"/api/v1/agent/conversations/"+convID+"/export", nil, true)
	if code != http.StatusOK {
		t.Fatalf("导出失败: %d %v", code, body)
	}
	turns, _ := body["turns"].([]any)
	if len(turns) != 1 {
		t.Fatalf("回合数 = %d", len(turns))
	}
	t0, _ := turns[0].(map[string]any)
	if t0["question"] != "说点什么" || t0["answer"] != "没什么可说的" {
		t.Errorf("导出内容不对: %v", t0)
	}

	// 导出必须能被序列化成一段干净的 JSON（供人存盘复核）。
	if _, err := json.Marshal(body); err != nil {
		t.Errorf("导出不可序列化: %v", err)
	}
}

// TestAgentToolsCatalog 验证「这个 Agent 能做什么」有独立出口。
//
// 刻意不调用 SetAgentConfig、也不建连接：工具契约是静态的。界面上「会做什么」
// 不该依赖「现在有没有连上库」——否则一个干净实例永远显示空清单，用户只能翻文档。
func TestAgentToolsCatalog(t *testing.T) {
	_, ts, _ := newServer(t)

	code, body := do(t, ts, http.MethodGet, "/api/v1/agent/tools", nil, true)
	if code != http.StatusOK {
		t.Fatalf("工具清单 = %d %v", code, body)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 14 {
		t.Fatalf("工具数 = %d，期望 14（docs/11 §3.2）", len(tools))
	}
	if n, _ := body["count"].(float64); int(n) != len(tools) {
		t.Errorf("count=%v 与清单长度 %d 不一致", body["count"], len(tools))
	}

	prev := ""
	for _, item := range tools {
		m, _ := item.(map[string]any)
		name, _ := m["name"].(string)
		if name == "" || m["description"] == "" {
			t.Errorf("工具缺名称或说明：%v", m)
			continue
		}
		if prev != "" && name <= prev {
			t.Errorf("清单要按名字排序（%s 出现在 %s 之后）：map 顺序随机会让这个端点每次响应都不同", name, prev)
		}
		prev = name

		params, _ := json.Marshal(m["parameters"])
		if !json.Valid(params) || len(params) < 2 {
			t.Errorf("工具 %s 的参数 schema 不是合法 JSON：%s", name, params)
		}
		// docs/11 验收 5：工具参数里不该出现「连接」这个概念，
		// 模型才无从把操作指向另一条连接。
		if strings.Contains(strings.ToLower(string(params)), "connection") {
			t.Errorf("工具 %s 的参数里出现了连接概念：%s", name, params)
		}
	}
}

// TestAgentConversationDetail 验证会话详情能查到元信息与已有记录。
func TestAgentConversationDetail(t *testing.T) {
	s, ts, dir := newServer(t)
	fake := &fakeModel{script: []string{textReply("没什么可说的")}}
	model := httptest.NewServer(fake.handler(t))
	defer model.Close()
	s.SetAgentConfig(api.AgentConfig{BaseURL: model.URL, Model: "fake"})

	connID := createConn(t, ts, dir, "test", "read-write")
	_, body := do(t, ts, http.MethodPost, "/api/v1/agent/conversations",
		map[string]any{"connection_id": connID, "mode": "read-only"}, true)
	convID, _ := body["id"].(string)
	if convID == "" {
		t.Fatal("没有返回会话 id")
	}

	code, body := do(t, ts, http.MethodGet, "/api/v1/agent/conversations/"+convID, nil, true)
	if code != http.StatusOK {
		t.Fatalf("详情 = %d %v", code, body)
	}
	if body["connection_id"] != connID || body["mode"] != "read-only" {
		t.Errorf("元信息不对：%v", body)
	}
	if body["running"] != false {
		t.Errorf("没在跑时应为 running=false，实际 %v", body["running"])
	}
	tr, _ := body["transcript"].(map[string]any)
	if turns, _ := tr["turns"].([]any); len(turns) != 0 {
		t.Errorf("新会话不该有记录：%v", turns)
	}

	if code, b := do(t, ts, http.MethodPost, "/api/v1/agent/conversations/"+convID+"/messages",
		map[string]any{"content": "说点什么"}, true); code != http.StatusOK {
		t.Fatalf("发消息失败: %d %v", code, b)
	}

	code, body = do(t, ts, http.MethodGet, "/api/v1/agent/conversations/"+convID, nil, true)
	if code != http.StatusOK {
		t.Fatalf("详情 = %d %v", code, body)
	}
	tr, _ = body["transcript"].(map[string]any)
	turns, _ := tr["turns"].([]any)
	if len(turns) != 1 {
		t.Fatalf("记录轮数 = %d，期望 1：%v", len(turns), tr)
	}
	turn, _ := turns[0].(map[string]any)
	if turn["question"] != "说点什么" {
		t.Errorf("记录里应带原问题：%v", turn)
	}
}

// TestAgentAbort 验证中止是一条真的取消，不是改个状态位。
//
// 假模型故意卡住不返回：这一轮只有在 ctx 被取消时才会结束。于是这个用例同时
// 证明了三件事——abort 真的传到了运行中的请求、messages 的响应是 200（不是
// 502「模型服务不可用」），以及停止原因写的是「中止」而不是「超时」。
func TestAgentAbort(t *testing.T) {
	s, ts, dir := newServer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		// 这里只负责「卡住不回应」。不用 r.Context().Done() 当放行条件：
		// 本机实测裸 net/http 客户端取消后服务端 ctx 数秒内都不触发（平台行为），
		// 用它做判据会让测试永远红，而且红得毫无信息。
		<-release
	}))
	defer model.Close()
	// 兜底放行：即便取消没传到这条连接，测试也不该卡在 Close 上——
	// 卡住只会把「取消没生效」伪装成超时，看不出真正的问题。
	defer close(release)
	s.SetAgentConfig(api.AgentConfig{BaseURL: model.URL, Model: "fake"})

	connID := createConn(t, ts, dir, "test", "read-write")
	_, body := do(t, ts, http.MethodPost, "/api/v1/agent/conversations",
		map[string]any{"connection_id": connID, "mode": "read-only"}, true)
	convID, _ := body["id"].(string)

	// 还没开始跑：必须明确回 409，而不是假装停掉了什么。
	code, body := do(t, ts, http.MethodPost, "/api/v1/agent/conversations/"+convID+"/abort", nil, true)
	if code != http.StatusConflict {
		t.Fatalf("空闲时中止 = %d %v，期望 409", code, body)
	}
	if er, _ := body["error"].(map[string]any); er["code"] != "CONVERSATION_IDLE" {
		t.Errorf("错误码 = %v", er["code"])
	}

	// 这一轮会在模型调用里卡住；用裸 http 在后台发，避免在子 goroutine 里碰 t。
	type msgResult struct {
		code int
		body map[string]any
		err  error
	}
	msgCh := make(chan msgResult, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agent/conversations/"+convID+"/messages",
			strings.NewReader(`{"content":"问点什么"}`))
		if err != nil {
			msgCh <- msgResult{err: err}
			return
		}
		req.Header.Set("X-DBRover-Token", testToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			msgCh <- msgResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var b map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&b)
		msgCh <- msgResult{code: resp.StatusCode, body: b}
	}()

	<-entered
	code, body = do(t, ts, http.MethodPost, "/api/v1/agent/conversations/"+convID+"/abort", nil, true)
	if code != http.StatusOK || body["aborted"] != true {
		t.Fatalf("中止 = %d %v", code, body)
	}

	// 中止要真的让那一轮尽快收尾：卡住只会让界面一直转圈。
	var res msgResult
	select {
	case res = <-msgCh:
	case <-time.After(2 * time.Second):
		t.Fatal("中止后 2 秒内这一轮仍未返回：取消没有生效")
	}
	if res.err != nil {
		t.Fatalf("messages 请求失败: %v", res.err)
	}
	if res.code != http.StatusOK {
		t.Fatalf("被中止的一轮应返回 200（结论与步骤要带回去），实际 %d: %v", res.code, res.body)
	}
	msg, _ := res.body["stop_message"].(string)
	if !strings.Contains(msg, "中止") {
		t.Errorf("停止原因要说清是「中止」，实际 %q（写成超时会让用户以为自己没点成功）", msg)
	}

	// 结束之后 running 要复位，否则界面会一直显示「运行中」。
	code, body = do(t, ts, http.MethodGet, "/api/v1/agent/conversations/"+convID, nil, true)
	if code != http.StatusOK || body["running"] != false {
		t.Errorf("中止后 running 应为 false：%d %v", code, body)
	}
}

// TestAgentCompactEndpoint 验证「手动压缩」有出口，且不会假装压过。
//
// 空会话压缩必须 skipped=true 并说明原因——如果它回一个「已压缩 0 条」的漂亮
// 数字，界面会以为腾出了空间。
func TestAgentCompactEndpoint(t *testing.T) {
	s, ts, dir := newServer(t)
	fake := &fakeModel{script: []string{textReply("嗯")}}
	model := httptest.NewServer(fake.handler(t))
	defer model.Close()
	s.SetAgentConfig(api.AgentConfig{BaseURL: model.URL, Model: "fake"})

	connID := createConn(t, ts, dir, "test", "read-write")
	_, body := do(t, ts, http.MethodPost, "/api/v1/agent/conversations",
		map[string]any{"connection_id": connID, "mode": "read-only"}, true)
	convID, _ := body["id"].(string)

	code, body := do(t, ts, http.MethodPost, "/api/v1/agent/conversations/"+convID+"/compact", nil, true)
	if code != http.StatusOK {
		t.Fatalf("压缩 = %d %v", code, body)
	}
	if body["skipped"] != true {
		t.Errorf("空历史应 skipped=true：%v", body)
	}
	if note, _ := body["note"].(string); note == "" {
		t.Errorf("跳过时要说明原因：%v", body)
	}
	if body["before"] != body["after"] {
		t.Errorf("跳过时不该改体积：%v", body)
	}

	if code, _ := do(t, ts, http.MethodPost, "/api/v1/agent/conversations/不存在/compact", nil, true); code != http.StatusNotFound {
		t.Errorf("未知会话应 404，实际 %d", code)
	}
}
