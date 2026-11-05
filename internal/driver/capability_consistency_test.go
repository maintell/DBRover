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

// TestUserTypesCapabilityIsHonest 是「别让能力位先说话」的绊线。
//
// 这张表被钉住过一次：PG 一直声明 UserTypes=true，而 Objects(KindType) 里根本
// 没有分支——能力位说 true、对象树里永远没有类型节点，会让「为什么看不到我的
// 枚举」变成一个查不出结论的问题。
//
// 翻 true 的顺序是固定的：先实现 Objects(KindType) → 加一条 drivertest 用例
// （建一个真类型再列出来）→ 再回来改这张表。PG 走完了这三步。
func TestUserTypesCapabilityIsHonest(t *testing.T) {
	want := map[model.Engine]bool{
		model.EngineSQLite:   false, // 没有用户类型
		model.EngineMySQL:    false, // ENUM 是列类型，不是独立对象
		model.EnginePostgres: true,  // pg_type 的枚举与域，已接进 Objects
	}
	for _, c := range []driver.Connector{sqlite.New(), mysql.New(), pg.New()} {
		exp, ok := want[c.Engine()]
		if !ok {
			t.Fatalf("新增引擎 %s 没写进这张表：用户类型能力要有明确预期", c.Engine())
		}
		if got := c.Capabilities().UserTypes; got != exp {
			t.Errorf("%s: Capabilities.UserTypes = %v，期望 %v（先实现 Objects(KindType) 再翻）", c.Engine(), got, exp)
		}
	}
}

// TestRoutineCapabilityIsHonest 钉住「例程列表」这句话的真值。
//
// 这条是同一个坑的第三次复用：能够列出来才算 true。MySQL 走
// information_schema.ROUTINES、PG 走 pg_proc（排除聚合、窗口函数与扩展成员），
// 两者都由 drivertest 的 RoutineListing 用真过程验证——建一个 → 列出来 →
// 取定义 → 删掉后消失 → 用定义回放。SQLite 没有例程，保持 false。
func TestRoutineCapabilityIsHonest(t *testing.T) {
	want := map[model.Engine]bool{
		model.EngineSQLite:   false, // 没有存储过程 / 函数
		model.EngineMySQL:    true,  // information_schema.ROUTINES
		model.EnginePostgres: true,  // pg_proc 的 prokind f/p
	}
	for _, c := range []driver.Connector{sqlite.New(), mysql.New(), pg.New()} {
		exp, ok := want[c.Engine()]
		if !ok {
			t.Fatalf("新增引擎 %s 没写进这张表：例程能力要有明确预期", c.Engine())
		}
		if got := c.Capabilities().Routines; got != exp {
			t.Errorf("%s: Capabilities.Routines = %v，期望 %v（先实现 Objects(KindRoutine) 再翻）", c.Engine(), got, exp)
		}
	}
}

// TestMetricsCapabilityIsHonest 钉住「性能面板」这句话的真值。
//
// PG 走 pg_stat_database + pg_settings，MySQL 走 SHOW GLOBAL STATUS +
// @@max_connections；SQLite 没有服务端的全局计数器（进程内单文件）。
// 翻 true 的顺序与其它能力位一致：先实现 → drivertest 的 MetricsReported
// 真机跑通 → 再回来改这张表。
func TestMetricsCapabilityIsHonest(t *testing.T) {
	want := map[model.Engine]bool{
		model.EngineSQLite:   false, // 没有服务端全局计数器
		model.EngineMySQL:    true,  // SHOW GLOBAL STATUS
		model.EnginePostgres: true,  // pg_stat_database
	}
	for _, c := range []driver.Connector{sqlite.New(), mysql.New(), pg.New()} {
		exp, ok := want[c.Engine()]
		if !ok {
			t.Fatalf("新增引擎 %s 没写进这张表：性能指标能力要有明确预期", c.Engine())
		}
		if got := c.Capabilities().Metrics; got != exp {
			t.Errorf("%s: Capabilities.Metrics = %v，期望 %v（先实现 MetricsProvider + MetricsReported 再翻）", c.Engine(), got, exp)
		}
	}
}

// TestSlowQueryCapabilityIsHonest 钉住「慢查询统计」这句话的真值。
//
// PG 走 pg_stat_statements（没开时降级并给打开步骤），MySQL 走
// performance_schema；SQLite 没有服务端统计。与其它能力位同一条规则：先实现 →
// 真机跑通 → 再翻这张表。
func TestSlowQueryCapabilityIsHonest(t *testing.T) {
	want := map[model.Engine]bool{
		model.EngineSQLite:   false,
		model.EngineMySQL:    true,
		model.EnginePostgres: true,
	}
	for _, c := range []driver.Connector{sqlite.New(), mysql.New(), pg.New()} {
		exp, ok := want[c.Engine()]
		if !ok {
			t.Fatalf("新增引擎 %s 没写进这张表：慢查询能力要有明确预期", c.Engine())
		}
		if got := c.Capabilities().SlowQueryStats; got != exp {
			t.Errorf("%s: Capabilities.SlowQueryStats = %v，期望 %v", c.Engine(), got, exp)
		}
	}
}
