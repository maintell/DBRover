package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/agent"
	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

// pgConn 把 SQLite 连接伪装成 PostgreSQL，让执行门选中 PG 方言。
// 跨库预检在碰连接之前就返回，所以这里不会真的跑 PG 语句。
type pgConn struct{ driver.Connector }

func (pgConn) Engine() model.Engine { return model.EnginePostgres }

// TestQueryToolSurfacesCrossDatabaseBlock 锁住「拦下之后要说出为什么」。
//
// 执行门返回 Blocked 而不是 error，若工具只看「有没有结果」，模型会收到
// EMPTY_RESULT + 「确认语句是否为查询」——方向完全错了，白烧一轮。
func TestQueryToolSurfacesCrossDatabaseBlock(t *testing.T) {
	conn := newConn(t, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)
	prov := &scripted{steps: []func(agent.Request) (agent.Response, error){
		callTool("query.execute", `{"sql":"SELECT * FROM other.public.t"}`),
		say("这条查询跨库了，改成另开连接"),
	}}
	rt := newRuntime(t, prov, pgConn{Connector: conn}, agent.ModeReadOnly)

	res, err := rt.Ask(context.Background(), "看一下 other 库里的 t")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(res.Steps) == 0 {
		t.Fatal("没有工具步骤")
	}
	step := res.Steps[0]
	if step.Error == nil {
		t.Fatalf("期望工具报错，实际 ok=%v", step.OK)
	}
	if step.Error.Code != "CROSS_DATABASE_UNSUPPORTED" {
		t.Errorf("错误码 = %q，期望 CROSS_DATABASE_UNSUPPORTED", step.Error.Code)
	}
	if !strings.Contains(step.Error.Message, "other.public.t") {
		t.Errorf("消息里要指出被拦的引用，实际 %q", step.Error.Message)
	}
	if step.Error.Hint == "" {
		t.Error("要给改法，hint 不能为空")
	}
}
