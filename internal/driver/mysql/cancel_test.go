package mysql_test

import (
	"context"
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/mysql"
	"github.com/maintell/DBRover/internal/model"
)

// TestCancelStopsStatementOnServer 锁住 MySQL 特有的那条约束（docs/05 验收 9）：
// 只取消本地 ctx 的话客户端会立刻返回，但 SLEEP() 不读 socket，服务端会继续
// 跑到自然结束。所以取消必须真的发 KILL QUERY。
func TestCancelStopsStatementOnServer(t *testing.T) {
	tgt := targetFromEnv(t)
	ctx := context.Background()

	c := mysql.New()
	if err := c.Open(ctx, driver.ConnectConfig{
		Host: tgt.host, Port: tgt.port, User: tgt.user, Password: tgt.pass,
		Environment: model.EnvTest, AccessMode: model.AccessReadWrite,
	}); err != nil {
		t.Fatalf("打开连接: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	sess, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = sess.Close() }()

	done := make(chan error, 1)
	go func() {
		_, err := sess.Execute(ctx, "SELECT SLEEP(30)", nil)
		done <- err
	}()

	// 给它一点时间真的开始跑，否则取消打在一个还没开始的语句上测不到东西。
	time.Sleep(600 * time.Millisecond)

	if err := sess.Cancel(ctx); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("取消后 8 秒仍未返回")
	}

	// 关键断言：换一条会话去看服务端，语句必须已经不在。
	probe, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("探测会话: %v", err)
	}
	defer func() { _ = probe.Close() }()

	const q = `SELECT COUNT(*) FROM information_schema.processlist
	  WHERE info LIKE '%SLEEP(30)%' AND id <> CONNECTION_ID()`
	var n int64
	deadline := time.Now().Add(3 * time.Second)
	for {
		cur, err := probe.Query(ctx, q, nil, driver.QueryOptions{})
		if err != nil {
			t.Fatalf("探测查询: %v", err)
		}
		rows, _, err := cur.Next(ctx)
		_ = cur.Close()
		if err != nil {
			t.Fatalf("读取探测结果: %v", err)
		}
		if len(rows) == 1 {
			n = toInt64(rows[0][0])
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("取消 3 秒后服务端仍有 %d 条 SLEEP 语句：KILL QUERY 没生效", n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case []byte:
		var n int64
		for _, b := range x {
			if b < '0' || b > '9' {
				break
			}
			n = n*10 + int64(b-'0')
		}
		return n
	default:
		return 0
	}
}
