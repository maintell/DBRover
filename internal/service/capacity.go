package service

import (
	"context"
	"fmt"
	"time"

	"github.com/maintell/DBRover/internal/driver"
)

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
	return cp.TableSizes(cctx, limit)
}
