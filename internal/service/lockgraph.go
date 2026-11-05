package service

import (
	"context"
	"fmt"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

// LockWaits 读当前的锁等待边（FR-MON-03）。
//
// 返回的是**边**而不是树：调用方要树可以自己按 BlockingID 串起来，而让每个驱动
// 各自拼树必然出现三种形状。没有等待时返回空切片。
func (e *Executor) LockWaits(ctx context.Context) ([]driver.LockWait, error) {
	if !e.conn.Capabilities().LockGraph {
		return nil, fmt.Errorf("%s 没有锁等待视图", e.conn.Engine())
	}
	lp, ok := e.conn.(driver.LockGraphProvider)
	if !ok {
		return nil, fmt.Errorf("能力位声明了锁等待，但 %s 驱动没有实现 LockGraphProvider", e.conn.Engine())
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return lp.LockWaits(cctx)
}
