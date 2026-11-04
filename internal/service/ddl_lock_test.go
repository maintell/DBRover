package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestHighRiskDDLBlockedByLockWait 补上 docs/07 验收第 11 条缺的那一步：
// 真的有人在等锁时，结构变更预案必须被拒（ErrLockWait），而不是排队加锁、
// 把队列拖得更长。
//
// 只跑 MySQL：造一个真实的锁等待需要「一个事务持锁 + 另一个事务排队」，
// 并且能查到 innodb_lock_waits；PG 的等价造法要用两个连接 + pg_locks，
// 目前没有对应钩子，不在这里假装覆盖。
func TestHighRiskDDLBlockedByLockWait(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		if tgt.name != "mysql" {
			continue
		}
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP TABLE IF EXISTS it_lock",
				"CREATE TABLE it_lock (id INT PRIMARY KEY, v INT)",
				"INSERT INTO it_lock (id, v) VALUES (1, 0)",
			}, nil)

			// 持锁方：一个开着的事务把行锁住。
			holder := tgt.open(t)
			hs, err := holder.Acquire(ctx)
			if err != nil {
				t.Fatalf("持锁连接: %v", err)
			}
			defer func() {
				_ = hs.Rollback(context.Background())
				_ = hs.Close()
			}()
			if err := hs.Begin(ctx); err != nil {
				t.Fatalf("Begin: %v", err)
			}
			cur, err := hs.Query(ctx, "SELECT v FROM it_lock WHERE id = 1 FOR UPDATE", nil, driver.QueryOptions{MaxRows: 1})
			if err != nil {
				t.Fatalf("持锁查询: %v", err)
			}
			_ = cur.Close()

			// 等锁方：另一个事务排在同一行上，制造 lock_wait。
			waitCtx, cancelWait := context.WithCancel(ctx)
			defer cancelWait()
			waiter := tgt.open(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				ws, err := waiter.Acquire(waitCtx)
				if err != nil {
					return
				}
				defer func() { _ = ws.Close() }()
				if err := ws.Begin(waitCtx); err != nil {
					return
				}
				c, err := ws.Query(waitCtx, "SELECT v FROM it_lock WHERE id = 1 FOR UPDATE", nil, driver.QueryOptions{MaxRows: 1})
				if err == nil {
					_, _, _ = c.Next(waitCtx)
					_ = c.Close()
				}
			}()
			defer func() {
				cancelWait()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
				}
			}()

			// 给锁等待一点时间出现在 innodb_lock_waits 里。
			time.Sleep(2 * time.Second)

			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()
			_, err = ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table:  tableRef("it_lock"),
				Action: service.DDLAddColumn,
				Column: service.ColumnSpec{Name: "note", Type: "VARCHAR(16)", Nullable: true},
			})
			if !errors.Is(err, service.ErrLockWait) {
				t.Fatalf("有人在等锁时预案应被拒（ErrLockWait），实际 err = %v", err)
			}
		})
	}
}
