package mysql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/mysql"
	"github.com/maintell/DBRover/internal/model"
)

// TestLockWaitsOnRealMySQL8 是 MySQL 8.0 锁等待的真机回归（可选）。
//
// 未设置 DBRover_TEST_MYSQL 时跳过，保证无数据库的机器 go test ./... 仍全绿。
// 指向 8.0 实例时，LockWaits 必须不再报
// 「Unknown table 'INNODB_LOCK_WAITS' in information_schema」——那是 8.0 移除
// information_schema.innodb_lock_waits 后旧驱动的固定报错。指向 5.7 也应通过
// （走 information_schema 路径）。当前无锁等待时返回空切片而不是错误。
//
// 只读：本用例只查询锁等待视图，不写任何数据。
func TestLockWaitsOnRealMySQL8(t *testing.T) {
	tgt := targetFromEnv(t)
	ctx := context.Background()
	c := mysql.New()
	if err := c.Open(ctx, driver.ConnectConfig{
		Host: tgt.host, Port: tgt.port, User: tgt.user, Password: tgt.pass,
		Environment: model.EnvTest, AccessMode: model.AccessReadOnly,
	}); err != nil {
		t.Fatalf("打开连接: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	lw, err := c.LockWaits(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "INNODB_LOCK_WAITS") {
			t.Fatalf("MySQL 8.0 上仍走了已移除的 information_schema.innodb_lock_waits: %v", err)
		}
		t.Fatalf("LockWaits 报错: %v", err)
	}
	if lw == nil {
		t.Error("无锁等待时应返回空切片而不是 nil")
	}
	for _, l := range lw {
		if l.WaitingID == "" || l.BlockingID == "" {
			t.Errorf("锁等待边缺少会话 id: %+v", l)
		}
	}
}
