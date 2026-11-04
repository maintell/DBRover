package driver

import "context"

// SessionInfo 是数据库侧一条会话（连接）的快照。
//
// 只放「运维要看的那几项」：谁、从哪来、在哪个库、在干什么、干了多久。
// 引擎的原始字段远不止这些，但列全了反而看不清。
type SessionInfo struct {
	// ID 是引擎侧的会话标识（PG 的 pid、MySQL 的 connection id），
	// 终止会话时原样传回去。
	ID string `json:"id"`
	// User 是登录用户。
	User string `json:"user,omitempty"`
	// Host 是客户端地址（引擎能给多少给多少）。
	Host string `json:"host,omitempty"`
	// Database 是当前库；空闲会话可能为空。
	Database string `json:"database,omitempty"`
	// State 是引擎自己报的状态（active / idle / Sleep / Query …）。
	State string `json:"state,omitempty"`
	// Query 是当前正在执行的语句；空闲时为空。
	Query string `json:"query,omitempty"`
	// DurationMS 是这条会话在当前状态下已持续的毫秒数。
	DurationMS int64 `json:"duration_ms"`
	// Idle 为 true 表示当前没在跑语句。
	Idle bool `json:"idle"`
}

// ValidSessionID 判断会话 id 的形态：只允许数字。
//
// 为什么放在 driver 包而不是各驱动自己写：KILL 这类语句在很多引擎里不接受参数
// 占位符，只能把 id 拼进语句；形态校验是拼之前唯一的防线，散在各处迟早漏一个。
func ValidSessionID(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// MonitorProvider 是「运行态观测」的可选能力（FR-MON-02）。
//
// 可选而不是主接口：没有这个概念或实现不了的引擎（SQLite 是单写者、没有会话
// 概念）不该为了实现而实现。上层必须先看 Capabilities().SessionMgmt，再用类型
// 断言取到它——两层判断的理由见 docs/04 §?：能力位是给 UI 的「要不要显示」，
// 断言是给代码的「能不能调」。
type MonitorProvider interface {
	// Sessions 列出当前会话（不含自己这条）。
	Sessions(ctx context.Context) ([]SessionInfo, error)
	// KillSession 强制结束一条会话。id 来自 Sessions 返回的 ID。
	//
	// 实现必须自己校验 id 的形态：KILL 这类语句在很多引擎里不接受参数占位符，
	// 只能拼进语句，绝不能让调用方把任意字符串塞进来。
	KillSession(ctx context.Context, id string) error
}
