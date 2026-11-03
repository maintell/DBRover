package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// longSQL 是一条会跑足够久的只读语句（SQLite 上递归到 5000 万行需要数秒）。
// 用它是因为取消必须在「语句还在引擎里跑」的时候发生才有意义。
const longSQL = `WITH RECURSIVE seq(n) AS (
  SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 50000000
) SELECT COUNT(*) FROM seq`

// TestCancelStopsRunningQuery 对应 docs/05 验收 9：
// 取消之后语句要停下来，而且不能等它自己跑完。
func TestCancelStopsRunningQuery(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)

	type result struct {
		out *service.Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := ex.Execute(context.Background(), service.Request{SQL: longSQL})
		done <- result{out: out, err: err}
	}()

	// 等它真的开始跑：Cancel 在「还没开始」时返回 false 是正确行为，
	// 但那样就测不到取消本身了。
	deadline := time.Now().Add(10 * time.Second)
	for !ex.Running() {
		if time.Now().After(deadline) {
			t.Fatal("10 秒内没有观察到正在执行的语句，长查询构造可能失效")
		}
		time.Sleep(5 * time.Millisecond)
	}

	started := time.Now()
	if !ex.Cancel(context.Background()) {
		t.Fatal("Cancel 应返回 true（确实有语句在跑）")
	}

	select {
	case r := <-done:
		elapsed := time.Since(started)
		if elapsed > 5*time.Second {
			t.Fatalf("取消后 %v 才返回，太慢", elapsed)
		}
		if r.err != nil {
			// Execute 在取消时仍应返回 outcome（不是 error），这里宽容一点。
			if !strings.Contains(strings.ToLower(r.err.Error()), "cancel") {
				t.Fatalf("取消后的错误不像取消: %v", r.err)
			}
			return
		}
		if r.out == nil {
			t.Fatal("没有 outcome")
		}
		msg := ""
		if r.out.Err != nil {
			if r.out.Err.Code != "SQL_CANCELED" {
				t.Fatalf("错误码 = %s，期望 SQL_CANCELED（消息：%s）", r.out.Err.Code, r.out.Err.Message)
			}
			msg = r.out.Err.Message
		} else if len(r.out.Results) > 0 {
			msg = r.out.Results[0].Err
		}
		if msg == "" {
			t.Fatal("取消之后不该报成功：结果里没有任何取消痕迹")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("取消后 10 秒仍未返回")
	}

	if ex.Running() {
		t.Error("跑完之后 Running() 应为 false")
	}
}

// TestCancelWhenIdleReturnsFalse 没有语句在跑时，取消是空操作而不是错误。
func TestCancelWhenIdleReturnsFalse(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	if ex.Cancel(context.Background()) {
		t.Error("空闲时 Cancel 应返回 false")
	}
	if ex.Running() {
		t.Error("空闲时 Running() 应为 false")
	}
}

// TestRunningClearedAfterNormalExecution 正常执行完之后也要清掉登记，
// 否则取消按钮会一直亮着，点到的是上一条已经跑完的语句。
func TestRunningClearedAfterNormalExecution(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	if _, err := ex.Execute(context.Background(), service.Request{SQL: "SELECT 1"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ex.Running() {
		t.Error("执行结束后不应还显示「在跑」")
	}
}

// TestCancelSurvivesInterleavedQuery 是真机验收里抓到的那个 bug：
// 长查询跑着的时候又发了一条短查询，短查询结束时把长查询的登记一起清了，
// 于是取消报「没在跑」。
//
// 现在按集合跟踪，各自注销自己的那一条。
func TestCancelSurvivesInterleavedQuery(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)

	done := make(chan *service.Outcome, 1)
	go func() {
		out, _ := ex.Execute(context.Background(), service.Request{SQL: longSQL})
		done <- out
	}()

	deadline := time.Now().Add(10 * time.Second)
	for !ex.Running() {
		if time.Now().After(deadline) {
			t.Fatal("10 秒内没有观察到正在执行的语句")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 插一条短查询并让它正常结束——这一步以前会把长查询的登记清掉。
	if _, err := ex.Execute(context.Background(), service.Request{SQL: "SELECT 1"}); err != nil {
		t.Fatalf("短查询失败: %v", err)
	}

	if !ex.Cancel(context.Background()) {
		t.Fatal("短查询结束后仍应能取消长查询")
	}

	select {
	case out := <-done:
		if out == nil || out.Err == nil || out.Err.Code != "SQL_CANCELED" {
			t.Fatalf("长查询应被取消，实际 %+v", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("取消后 10 秒仍未返回")
	}
}
