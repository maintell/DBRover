package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// pgLabelled 把 SQLite 连接伪装成 PostgreSQL，只为让执行门选中 PG 方言。
//
// 这些用例根本走不到真实执行：跨库预检在碰连接之前就拦下了。用它比为了
// 一条预检去起一个 PG 实例更省事，也不会因为「方言判断写错」而被 SQLite
// 的宽容掩盖。
type pgLabelled struct {
	driver.Connector
	engine model.Engine
}

func (c pgLabelled) Engine() model.Engine { return c.engine }

// TestCrossDatabaseReferenceIsBlocked 验收 docs/05 第 12 条：
// PG 的跨库引用要在执行前拦下并说明原因，而不是把引擎的原始报错甩给用户。
func TestCrossDatabaseReferenceIsBlocked(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(pgLabelled{Connector: e.rw, engine: model.EnginePostgres},
		model.EnvTest, model.AccessReadWrite, nil)
	ctx := context.Background()

	out, err := ex.Execute(ctx, service.Request{SQL: `SELECT * FROM other.public.t`})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !out.Blocked {
		t.Fatal("跨库引用应在执行前被拦下")
	}
	if out.BlockCode != "CROSS_DATABASE_UNSUPPORTED" {
		t.Errorf("错误码 = %q，期望 CROSS_DATABASE_UNSUPPORTED", out.BlockCode)
	}
	if !strings.Contains(out.Reason, "other.public.t") {
		t.Errorf("原因里要指出被拦的引用，实际 %q", out.Reason)
	}
	if out.BlockHint == "" {
		t.Error("要说清怎么办（另开连接 / postgres_fdw / dblink），hint 不能为空")
	}
	if len(out.Results) != 0 {
		t.Errorf("拦下时不该有任何语句被执行，实际 %d 条结果", len(out.Results))
	}

	// 不跨库的两段名必须照常放行——预检不能把正常语句一起挡了。
	ok, err := ex.Execute(ctx, service.Request{SQL: `SELECT id FROM t`})
	if err != nil {
		t.Fatalf("正常语句执行失败: %v", err)
	}
	if ok.Blocked {
		t.Fatalf("两段名被误拦：%s", ok.Reason)
	}

	// 字符串里的三段名是数据，不是引用。
	lit, err := ex.Execute(ctx, service.Request{SQL: `SELECT 'other.public.t' AS s`})
	if err != nil {
		t.Fatalf("字符串语句执行失败: %v", err)
	}
	if lit.Blocked {
		t.Fatalf("字符串里的三段名被误拦：%s", lit.Reason)
	}
}

// TestCrossDatabaseOnlyOnPostgres 确认这条预检不会误伤别的引擎：
// MySQL 的 `库.表`、SQL Server 的 `库.模式.表` 都是正常写法。
//
// 这里只断言「不是因为跨库被拦」——`a.b.c` 在 SQLite 上本来也查不到，
// 那是执行期错误，走错误模型，不该被伪装成策略拦截。
func TestCrossDatabaseOnlyOnPostgres(t *testing.T) {
	e := setup(t)
	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	out, err := ex.Execute(context.Background(), service.Request{SQL: `SELECT id FROM a.b.c`})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.BlockCode == "CROSS_DATABASE_UNSUPPORTED" {
		t.Fatalf("SQLite 上不该因为三段名被预检拦下：%s", out.Reason)
	}
}
