// Package host 装配「一条 DBRover 进程」所需的全部依赖，供 cmd/dbrover 与
// cmd/dbrover-desktop 共用。两种形态复用同一份 api.Server、执行门、审计与界面。
package host

import (
	"context"
	"fmt"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/mysql"
	"github.com/maintell/DBRover/internal/driver/pg"
	"github.com/maintell/DBRover/internal/driver/redis"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
)

// OpenConnector 按引擎分发连接器工厂。api 层只接受函数，不认识具体驱动；
// 从 cmd/dbrover 抽到这里，桌面壳与 CLI 用同一份，避免两处工厂各自演化。
func OpenConnector(engine model.Engine, cfg driver.ConnectConfig) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch engine {
	case model.EngineSQLite:
		c := sqlite.New()
		if err := c.Open(ctx, cfg); err != nil {
			return nil, err
		}
		return c, nil
	case model.EngineMySQL, model.EngineMariaDB:
		c := mysql.New()
		if err := c.Open(ctx, cfg); err != nil {
			return nil, err
		}
		return c, nil
	case model.EnginePostgres:
		c := pg.New()
		if err := c.Open(ctx, cfg); err != nil {
			return nil, err
		}
		return c, nil
	case model.EngineRedis:
		// 非关系型：实现的是 driver.KVConnector，不是 driver.Connector（ADR-0007）。
		c := redis.New()
		if err := c.Open(ctx, cfg); err != nil {
			return nil, err
		}
		return c, nil
	default:
		return nil, fmt.Errorf("暂不支持的引擎: %s", engine)
	}
}
