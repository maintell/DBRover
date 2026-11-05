package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

// ErrCollectTimeout 表示采集超时。
//
// docs/09 §2.1 与验收 8：**超时不算错误**，界面记一次降级。把它做成哨兵错误而不是
// 让每个 handler 各自判 ctx.Err()，是因为判据必须一致——漏一处就会出现「同一个面板，
// 慢查询超时是黄色提示、体量超时是 502」。
var ErrCollectTimeout = errors.New("采集超时")

// Capacity 读表体量 Top N（FR-MON-06）。
//
// 只给体量、不给增长：增长要有本地时序留存（FR-MON-05），那是另一件事。
func (e *Executor) Capacity(ctx context.Context, limit int) ([]driver.TableSize, error) {
	if !e.conn.Capabilities().Capacity {
		return nil, fmt.Errorf("%s 没有表体量视图", e.conn.Engine())
	}
	cp, ok := e.conn.(driver.CapacityProvider)
	if !ok {
		return nil, fmt.Errorf("能力位声明了表体量，但 %s 驱动没有实现 CapacityProvider", e.conn.Engine())
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	items, err := cp.TableSizes(cctx, limit)
	if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(cctx.Err(), context.DeadlineExceeded)) {
		return nil, fmt.Errorf("%w：表体量采集超过 3 秒", ErrCollectTimeout)
	}
	return items, err
}
