package driver_test

import (
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/dialect"
	"github.com/maintell/DBRover/internal/driver/mysql"
	"github.com/maintell/DBRover/internal/driver/pg"
	"github.com/maintell/DBRover/internal/driver/sqlite"
	"github.com/maintell/DBRover/internal/model"
)

// TestExplainCapabilityMatchesDialect 守住 ADR-0005：能力声明是唯一来源。
//
// 三个引擎的执行计划早就接好了，Capabilities.Explain 却一直是 false——
// 结果界面与调用方只能靠「点一次看报不报错」来判断能不能用，而 docs/04 §8
// 的清单要求「能力有无进 Capabilities，UI 据此决定摆不摆入口」。
// 这条同时挡住反方向：方言说支持、声明说支持，二者不许各说各话。
func TestExplainCapabilityMatchesDialect(t *testing.T) {
	for _, c := range []driver.Connector{sqlite.New(), mysql.New(), pg.New()} {
		_, supported := dialect.For(c.Engine()).ExplainSQL("SELECT 1")
		if !supported {
			t.Errorf("%s: 方言应当支持 ExplainSQL", c.Engine())
		}
		if got := c.Capabilities().Explain; got != supported {
			t.Errorf("%s: Capabilities.Explain = %v，方言支持 = %v", c.Engine(), got, supported)
		}
	}
	// 未实现的引擎必须老实说不支持，而不是等执行期才发现。
	for _, e := range []model.Engine{model.EngineMSSQL, model.EngineOracle} {
		if _, ok := dialect.For(e).ExplainSQL("SELECT 1"); ok {
			t.Errorf("%s: 方言不该声称支持执行计划", e)
		}
	}
}

// TestTriggerCapabilityIsHonest 钉住「触发器列表」这句话的真值。
//
// 这三个引擎现在都真的能列：SQLite 走 sqlite_master、MySQL 走
// information_schema.TRIGGERS、PG 走 pg_trigger（排除 tgisinternal，否则每个
// 外键都会冒出两条内部触发器）。声明为 true 是有实现兜着的——drivertest 的
// TriggerListing 用各引擎自己的钩子建一个真触发器再列出来。
func TestTriggerCapabilityIsHonest(t *testing.T) {
	want := map[model.Engine]bool{
		model.EngineSQLite:   true, // sqlite_master
		model.EngineMySQL:    true, // information_schema.TRIGGERS
		model.EnginePostgres: true, // pg_trigger
	}
	for _, c := range []driver.Connector{sqlite.New(), mysql.New(), pg.New()} {
		exp, ok := want[c.Engine()]
		if !ok {
			t.Fatalf("新增引擎 %s 没写进这张表：触发器能力要有明确预期", c.Engine())
		}
		if got := c.Capabilities().Triggers; got != exp {
			t.Errorf("%s: Capabilities.Triggers = %v，期望 %v", c.Engine(), got, exp)
		}
	}
}

// TestSequenceCapabilityIsHonest 同理：PG 的序列就是 pg_class 里 relkind='S'
// 的对象，已接进 Objects 并由 drivertest 的 SequenceListing 用真序列验证；
// SQLite / MySQL 本工具还没接，能力位保持 false。
func TestSequenceCapabilityIsHonest(t *testing.T) {
	want := map[model.Engine]bool{
		model.EngineSQLite:   false, // 没有序列这个概念
		model.EngineMySQL:    false, // 未接
		model.EnginePostgres: true,  // pg_class relkind='S'
	}
	for _, c := range []driver.Connector{sqlite.New(), mysql.New(), pg.New()} {
		exp, ok := want[c.Engine()]
		if !ok {
			t.Fatalf("新增引擎 %s 没写进这张表：序列能力要有明确预期", c.Engine())
		}
		if got := c.Capabilities().Sequences; got != exp {
			t.Errorf("%s: Capabilities.Sequences = %v，期望 %v", c.Engine(), got, exp)
		}
	}
}
