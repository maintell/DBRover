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
