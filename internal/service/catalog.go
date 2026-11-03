package service

import (
	"context"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

// Catalog 是只读元数据的门面。
//
// 存在的理由不是「包装一层」，而是分层：AGENTS.md 规定 agent 只能通过 service
// 触达数据，而元数据读取此前散在 api 层直接调 driver。有了这一层，agent 不必
// import driver，也就不会不知不觉绕开执行门去直接查库。
//
// 这里的方法一律只读，没有副作用，也不记审计——读元数据不是数据操作。
type Catalog struct {
	conn driver.Connector
}

// NewCatalog 为一条已打开的连接构造元数据门面。
func NewCatalog(c driver.Connector) *Catalog { return &Catalog{conn: c} }

// Engine 返回引擎类型。
func (c *Catalog) Engine() model.Engine { return c.conn.Engine() }

// Capabilities 返回该引擎支持的能力位。
func (c *Catalog) Capabilities() model.Capabilities { return c.conn.Capabilities() }

// Health 返回服务器版本与身份信息。连不通时返回错误，调用方自行决定降级。
func (c *Catalog) Health(ctx context.Context) (*driver.HealthInfo, error) {
	return c.conn.Health(ctx)
}

// Catalogs 列出库（MySQL 的 database、PG 的 database、SQLite 的附加库）。
func (c *Catalog) Catalogs(ctx context.Context) ([]string, error) {
	return c.conn.Meta().Catalogs(ctx)
}

// Schemas 列出指定库下的模式。
func (c *Catalog) Schemas(ctx context.Context, catalog string) ([]string, error) {
	return c.conn.Meta().Schemas(ctx, catalog)
}

// Objects 列出对象。kinds 为空表示全部类型。
func (c *Catalog) Objects(ctx context.Context, catalog, schema string, kinds ...model.ObjectKind) ([]model.ObjectRef, error) {
	return c.conn.Meta().Objects(ctx, catalog, schema, kinds...)
}

// DescribeTable 返回表/视图的完整结构。
func (c *Catalog) DescribeTable(ctx context.Context, ref model.ObjectRef) (*model.TableDetail, error) {
	return c.conn.Meta().DescribeTable(ctx, ref)
}

// Search 按名称模糊搜索对象。
func (c *Catalog) Search(ctx context.Context, pattern string, limit int) ([]model.ObjectRef, error) {
	return c.conn.Meta().Search(ctx, pattern, limit)
}

// Dependents 返回引用某对象的其他对象（视图、外键）。
func (c *Catalog) Dependents(ctx context.Context, ref model.ObjectRef) ([]model.Dependent, error) {
	return c.conn.Meta().Dependents(ctx, ref)
}

// Blockers 返回可能让 DDL 卡住的会话（长事务、等待锁）。
func (c *Catalog) Blockers(ctx context.Context, threshold time.Duration) ([]model.Blocker, error) {
	return c.conn.Meta().Blockers(ctx, threshold)
}
