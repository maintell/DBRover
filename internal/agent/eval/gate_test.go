package eval

import (
	"testing"

	"github.com/maintell/DBRover/internal/agent"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestEveryWriteToolHasAGateCase 是「写操作 100% 过门禁」在**评测集**这一侧的检查。
//
// docs/11 验收第 2 条要求「全评测集」覆盖写路径。人工记账不算数：这里直接对着
// 真实注册表问「有哪些写工具」，再要求每一个都至少有一条用例真的调用了它、
// 并且断言了预期的拒绝码。以后加写工具却忘了补用例，这条会红。
func TestEveryWriteToolHasAGateCase(t *testing.T) {
	tgt := OpenSQLite(t)
	rt := evalRuntime(t, tgt, agent.ModeWriteWithApproval)

	known := map[string]bool{}
	var writes []string
	for _, m := range rt.ToolMetas() {
		known[m.Name] = true
		if m.Write {
			writes = append(writes, m.Name)
		}
	}
	if len(writes) == 0 {
		t.Fatal("注册表里没有写工具：这条检查等于没跑")
	}

	cases := Cases()
	for _, c := range cases {
		for _, resp := range c.Script {
			for _, call := range resp.ToolCalls {
				if !known[call.Name] {
					t.Errorf("%s 调用了不存在的工具 %s（名字拼错时用例只是「恰好」不违规）", c.ID, call.Name)
				}
			}
		}
	}

	for _, w := range writes {
		covered := ""
		for _, c := range cases {
			if len(c.MustDeny) == 0 || !scriptCalls(c, w) {
				continue
			}
			covered = c.ID
			break
		}
		if covered == "" {
			t.Errorf("写工具 %s 没有任何「预期被拒」的用例覆盖，写门禁在评测集里是空白", w)
		}
	}
}

// evalRuntime 按 Run 的方式建一个运行时，只用于读取工具注册表。
func evalRuntime(t *testing.T, tgt Target, mode agent.Mode) *agent.Runtime {
	t.Helper()
	rt, err := agent.NewRuntime(agent.Options{
		Provider:    &replay{steps: []agent.Response{Say("ok")}},
		Executor:    service.NewExecutor(tgt.Conn, model.EnvTest, model.AccessReadWrite, nil),
		Catalog:     service.NewCatalog(tgt.Conn),
		Mode:        mode,
		Environment: model.EnvTest,
		AccessMode:  model.AccessReadWrite,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return rt
}

// scriptCalls 判断某条用例的录制脚本里是否调用了指定工具。
func scriptCalls(c Case, name string) bool {
	for _, resp := range c.Script {
		for _, call := range resp.ToolCalls {
			if call.Name == name {
				return true
			}
		}
	}
	return false
}
