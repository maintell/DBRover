package service

import (
	"context"

	"github.com/maintell/DBRover/internal/driver"
)

// runningExec 是「此刻在这个连接上跑着的一条」。
//
// 用集合而不是单个槽位：一条连接上可能同时有多条在跑（界面上是串行的，
// 但 API 调用方不一定）。只记一条会踩到两个坑——后开始的那条把前一条的登记
// 顶掉，前一条结束时又把后一条的登记清空，结果取消要么找错对象、要么说「没在跑」。
// 这个坑是真机验收时暴露出来的：中间插了一次探测查询，取消就报了 canceled=false。
type runningExec struct {
	sess   driver.Session
	cancel context.CancelFunc
}

// beginRun 登记一条正在执行的语句，返回带独立取消的 ctx 与注销函数。
//
// 为什么要独立的 cancel 而不是只靠请求 ctx：用户按「取消」时，浏览器那条
// 请求并不会断开——它正等着结果。所以必须有一个服务端可控的取消信号。
func (e *Executor) beginRun(ctx context.Context, sess driver.Session) (context.Context, func()) {
	runCtx, cancel := context.WithCancel(ctx)
	rec := &runningExec{sess: sess, cancel: cancel}

	e.runMu.Lock()
	if e.running == nil {
		e.running = map[*runningExec]struct{}{}
	}
	e.running[rec] = struct{}{}
	e.runMu.Unlock()

	return runCtx, func() {
		e.runMu.Lock()
		delete(e.running, rec)
		e.runMu.Unlock()
		cancel()
	}
}

// Cancel 终止这条连接上正在执行的语句，返回是否真的取消了东西。
//
// 返回 false 表示此刻没有语句在跑（用户按晚了，或者已经跑完了）——
// 这不是错误，界面上不该因此弹红字。
//
// 取消的是**这个连接上所有还在跑的语句**，不是「最新那一条」：用户按下取消时
// 想的是「这个连接别再算了」，按「最新」去猜只会在并发场景下取消错对象。
//
// 两件事都做：先让会话自己取消（驱动层中断当前调用），再取消 ctx
// （让还阻塞在客户端的那一步立刻返回）。
//
// 三个引擎都做到「引擎侧真的停」，但原理不同：
//   - SQLite：interrupt，立刻停。
//   - PostgreSQL：客户端断开后服务端在检查点发现 EOF 并终止语句。
//   - MySQL：SLEEP() 之类不读 socket 的语句不会因为客户端断开而停止，
//     所以驱动侧另发一条 KILL QUERY <id>（会话为此钉在单条物理连接上）。
func (e *Executor) Cancel(ctx context.Context) bool {
	e.runMu.Lock()
	recs := make([]*runningExec, 0, len(e.running))
	for r := range e.running {
		recs = append(recs, r)
	}
	e.runMu.Unlock()

	if len(recs) == 0 {
		return false
	}
	for _, r := range recs {
		_ = r.sess.Cancel(ctx)
		r.cancel()
	}
	return true
}

// Running 报告此刻是否有语句在跑，供界面决定要不要显示取消按钮。
func (e *Executor) Running() bool {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	return len(e.running) > 0
}
