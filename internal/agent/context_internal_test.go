package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestQuestionTokens 检索词要能挡住噪音，又不能误杀真表名。
func TestQuestionTokens(t *testing.T) {
	got := questionTokens("帮我看看 orders 表里面 user_id 有多少")
	// 「帮我」「看看」「多少」「表里」都是虚词；orders / user_id 才是要搜的东西。
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "orders") {
		t.Errorf("应保留 orders，实际 %v", got)
	}
	if !strings.Contains(joined, "user_id") {
		t.Errorf("应保留 user_id，实际 %v", got)
	}
	for _, bad := range []string{"帮我", "看看", "多少", "表里"} {
		for _, g := range got {
			if g == bad {
				t.Errorf("虚词 %q 不该进检索词: %v", bad, got)
			}
		}
	}

	if n := len(questionTokens("a b c d e f g h")); n > maxRetrievalTokens {
		t.Errorf("检索词数量应被截断，实际 %d", n)
	}
	if got := questionTokens("the and select from"); len(got) != 0 {
		t.Errorf("全是英文虚词时不该有检索词: %v", got)
	}

	// 中文没有空格：长串要给短前缀，否则 LIKE 一条都匹配不到。
	cjk := questionTokens("订单表有多少条")
	if !containsStr(cjk, "订单") {
		t.Errorf("长中文串应给出 2 字前缀，实际 %v", cjk)
	}
}

// TestRetrieveContext 用真 SQLite 元数据验证：问题里点名的表能被检索到，
// 并且带上列定义；点名的表不存在时不要硬凑背景。
func TestRetrieveContext(t *testing.T) {
	conn := openInnerConn(t,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, amount INTEGER)`,
		`CREATE TABLE unrelated (id INTEGER PRIMARY KEY)`,
	)
	rt := newInnerRuntime(t, conn, ModeReadOnly)

	got := rt.retrieveContext(context.Background(), "orders 里 user_id 有多少")
	if !strings.Contains(got, "orders") {
		t.Fatalf("应检索到 orders: %q", got)
	}
	if !strings.Contains(got, "user_id") {
		t.Errorf("应带上列定义: %q", got)
	}
	if strings.Contains(got, "unrelated") {
		t.Errorf("不该把无关表塞进来: %q", got)
	}

	// 认不出的词不该产出背景（宁可不给，也不给错的）。
	if s := rt.retrieveContext(context.Background(), "zzz_nothing_here"); s != "" {
		t.Errorf("没有命中时不该有背景: %q", s)
	}
}

// TestCompactHistoryKeepsNewest 压缩只动最早的工具结果，且必须留一句说明。
func TestCompactHistoryKeepsNewest(t *testing.T) {
	big := strings.Repeat("x", 5000)
	msgs := []Message{
		{Role: "user", Content: "开始"},
		{Role: "tool", Name: "query.execute", ToolCallID: "1", Content: big},
		{Role: "assistant", Content: "看了看"},
		{Role: "tool", Name: "query.execute", ToolCallID: "2", Content: big},
	}
	out, n := compactHistory(msgs, 6000)
	if n == 0 {
		t.Fatal("超出预算时应发生压缩")
	}
	if !strings.Contains(out[1].Content, "已压缩") {
		t.Errorf("被压缩的那条必须留说明: %q", out[1].Content[:80])
	}
	// 最新一条不动：刚发生的事压掉，模型最容易重复调用。
	if out[3].Content != big {
		t.Error("最新一条工具结果不该被压缩")
	}
	// 非工具消息不动。
	if out[0].Content != "开始" || out[2].Content != "看了看" {
		t.Error("对话与结论不该被压缩")
	}

	// 预算够时什么都不做。
	if _, n := compactHistory(msgs, 100000); n != 0 {
		t.Errorf("预算充足时不该压缩，实际压了 %d 条", n)
	}
}

// TestAskInjectsRetrievedContext 端到端：问题里点名的表要出现在模型收到的消息里。
func TestAskInjectsRetrievedContext(t *testing.T) {
	conn := openInnerConn(t, `CREATE TABLE orders (id INTEGER PRIMARY KEY, amount INTEGER)`)
	prov := &captureProvider{}
	rt, err := NewRuntime(Options{
		Provider:    prov,
		Executor:    service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil),
		Catalog:     service.NewCatalog(conn),
		Mode:        ModeReadOnly,
		Environment: model.EnvTest,
		AccessMode:  model.AccessReadWrite,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	if _, err := rt.Ask(context.Background(), "orders 表有多少行"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	var found bool
	for _, m := range prov.last.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "orders") {
			found = true
		}
	}
	if !found {
		t.Fatalf("发给模型的消息里应含检索到的 orders 背景: %+v", prov.last.Messages)
	}
	if len(prov.last.Tools) != 14 {
		t.Errorf("工具数 = %d，期望 14", len(prov.last.Tools))
	}
}

// captureProvider 记下最后一次请求，方便断言「发给模型的到底是什么」。
type captureProvider struct {
	last Request
}

func (p *captureProvider) Name() string       { return "capture" }
func (p *captureProvider) Caps() ProviderCaps { return ProviderCaps{ToolCalling: true} }
func (p *captureProvider) Complete(_ context.Context, req Request) (Response, error) {
	p.last = req
	return Response{Text: "好的"}, nil
}
func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestRetrievalScale 对应 docs/11 §13 第 7 项：大库上「不全量注入」要是真的，
// 而不是写个注释就算。这里造 2000 张表，断言背景文本 < 3k 字符且检索不慢。
func TestRetrievalScale(t *testing.T) {
	// 建表必须放在一个事务里：SQLite 每条 DDL 单独提交时每次都要 fsync，
	// 2000 张表能跑几分钟——那是测试写法的问题，不是被测代码的问题。
	conn := openInnerConn(t, `CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER)`)
	seedManyTables(t, conn, 2000)
	rt := newInnerRuntime(t, conn, ModeReadOnly)

	start := time.Now()
	got := rt.retrieveContext(context.Background(), "orders 有多少行")
	elapsed := time.Since(start)

	if !strings.Contains(got, "orders") {
		t.Fatalf("2000 张表里应检索到 orders: %q", got[:min(200, len(got))])
	}
	if len(got) > 3000 {
		t.Errorf("背景文本 %d 字符，超过 3000：说明没有截断", len(got))
	}
	if elapsed > time.Second {
		t.Errorf("检索耗时 %v，超过 1s", elapsed)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// seedManyTables 在一个事务里批量建表，避免逐条提交的 fsync 拖慢用例。
func seedManyTables(t *testing.T, conn driver.Connector, n int) {
	t.Helper()
	ctx := context.Background()
	sess, err := conn.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if err := sess.Begin(ctx); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i := 0; i < n; i++ {
		q := fmt.Sprintf(`CREATE TABLE t_%04d (id INTEGER PRIMARY KEY, v TEXT)`, i)
		if _, err := sess.Execute(ctx, q, nil); err != nil {
			t.Fatalf("建表 %d: %v", i, err)
		}
	}
	if err := sess.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// TestRetrievalNotPersistedInHistory 检索背景只服务于当轮：不能堆进历史。
//
// 不这么做的后果是多轮之后预算被它吃光，而压缩只动工具结果，压不掉它。
func TestRetrievalNotPersistedInHistory(t *testing.T) {
	conn := openInnerConn(t, `CREATE TABLE orders (id INTEGER PRIMARY KEY)`)
	prov := &captureProvider{}
	rt, err := NewRuntime(Options{
		Provider:    prov,
		Executor:    service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil),
		Catalog:     service.NewCatalog(conn),
		Mode:        ModeReadOnly,
		Environment: model.EnvTest,
		AccessMode:  model.AccessReadWrite,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	if _, err := rt.Ask(context.Background(), "orders 有多少行"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	// 这一轮发给模型的确实有背景。
	if !hasContextMessage(prov.last.Messages) {
		t.Fatal("当轮应注入检索背景")
	}

	// 下一轮：历史里不该再带着上一轮的背景。
	prov.last = Request{}
	if _, err := rt.Ask(context.Background(), "再问一次 orders"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	contextBlocks := 0
	for _, m := range prov.last.Messages {
		if m.Role == "system" && m.Name == "context" {
			contextBlocks++
		}
	}
	if contextBlocks != 1 {
		t.Fatalf("历史里应只有本轮的一份背景，实际 %d 份", contextBlocks)
	}
}

func hasContextMessage(msgs []Message) bool {
	for _, m := range msgs {
		if m.Role == "system" && m.Name == "context" {
			return true
		}
	}
	return false
}
