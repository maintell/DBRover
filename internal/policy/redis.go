package policy

import (
	"strings"

	"github.com/maintell/DBRover/internal/model"
)

// RedisCommandRisk 给一条 Redis 命令定风险等级。
//
// 为什么不复用 SQL 分级：SQL 分级要先过 dialect 的词法与语句分类，Redis 命令
// 没有语句结构，只有命令名与参数。硬套会得到「所有命令都是 R2」这种等于没有
// 分级的结论（ADR-0007 §3）。
//
// **不在表里的命令一律 R4**，与「unknown 环境等同 production」同一条原则：
// 未知的东西按最危险处理，让人显式确认，而不是默认放行。
func RedisCommandRisk(name string) (Level, Reason) {
	cmd := strings.ToUpper(strings.TrimSpace(name))
	if cmd == "" {
		return R4, ReasonUnknownCommand
	}

	switch cmd {
	// ---- 只读、单点、成本可控 ----
	case "GET", "MGET", "EXISTS", "TTL", "PTTL", "TYPE", "STRLEN",
		"HGET", "HMGET", "HGETALL", "HEXISTS", "HLEN", "HKEYS", "HVALS",
		"LLEN", "LRANGE", "LINDEX",
		"SCARD", "SISMEMBER", "SMEMBERS",
		"ZCARD", "ZSCORE", "ZRANK", "ZRANGE", "ZRANGEBYSCORE", "ZCOUNT",
		"XLEN", "XRANGE", "XREVRANGE",
		"GETRANGE", "BITCOUNT", "PFCOUNT",
		"INFO", "DBSIZE", "PING", "TIME", "LASTSAVE", "SLOWLOG":
		return R0, ReasonReadOnly

	// ---- 只读但有遍历成本：大 keyspace / 大 key 上会明显变慢 ----
	case "SCAN", "HSCAN", "SSCAN", "ZSCAN", "XINFO", "MEMORY", "OBJECT", "RANDOMKEY":
		return R1, ReasonTraversal

	// ---- 单 key 写 ----
	case "SET", "SETNX", "SETEX", "PSETEX", "GETSET", "GETDEL", "APPEND", "SETRANGE",
		"HSET", "HMSET", "HSETNX", "HINCRBY", "HDEL",
		"LPUSH", "RPUSH", "LPOP", "RPOP", "LSET", "LTRIM", "LREM", "LINSERT",
		"SADD", "SREM", "SPOP",
		"ZADD", "ZREM", "ZINCRBY", "ZREMRANGEBYRANK", "ZREMRANGEBYSCORE",
		"XADD", "XDEL", "XTRIM",
		"INCR", "DECR", "INCRBY", "DECRBY", "INCRBYFLOAT",
		"EXPIRE", "PEXPIRE", "EXPIREAT", "PERSIST":
		return R2, ReasonSingleKeyWrite

	// ---- 删除 / 改名：数据真的没了，但范围仍是单 key ----
	case "DEL", "UNLINK", "RENAME", "RENAMENX", "MOVE", "COPY", "DUMP", "RESTORE":
		return R2, ReasonSingleKeyWrite

	// ---- 会阻塞实例：能用但不该随手用 ----
	case "KEYS", "SORT", "SUNION", "SINTER", "SDIFF", "SUNIONSTORE", "SINTERSTORE", "SDIFFSTORE":
		return R3, ReasonExpensive

	// ---- 整库 / 整实例 ----
	case "FLUSHDB", "FLUSHALL", "SWAPDB":
		return R4, ReasonDestructive

	// ---- 改变实例行为或执行任意代码 ----
	case "CONFIG", "DEBUG", "SHUTDOWN", "SCRIPT", "EVAL", "EVALSHA", "FUNCTION", "MODULE", "ACL", "SAVE", "BGSAVE", "BGREWRITEAOF":
		return R4, ReasonAdminCommand

	// ---- 影响别人或拓扑 ----
	case "CLIENT", "REPLICAOF", "SLAVEOF", "MIGRATE", "CLUSTER", "FAILOVER", "MONITOR":
		return R4, ReasonAdminCommand
	}

	return R4, ReasonUnknownCommand
}

// Reason 说明这条命令为什么是这个等级，给界面直接用。
type Reason string

const (
	ReasonReadOnly       Reason = "只读"
	ReasonTraversal      Reason = "只读，但要遍历，大 key 上会明显变慢"
	ReasonSingleKeyWrite Reason = "写单个 key"
	ReasonExpensive      Reason = "会阻塞实例，大 keyspace 上尤其明显"
	ReasonDestructive    Reason = "清空整库或整个实例"
	ReasonAdminCommand   Reason = "改变实例行为或执行任意代码"
	ReasonUnknownCommand Reason = "未知命令，按最危险处理"
)

// RedisCommandBlocked 判断这条命令是否应当直接拒绝（不给审批机会）。
//
// 只有两类：KEYS（一定阻塞且有完全等价的 SCAN）与 DEBUG（调试后门）。
// 其余高危命令仍然允许，但要审批——「不允许」和「要确认」是两件事。
func RedisCommandBlocked(name string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "KEYS":
		return "KEYS 会阻塞整个实例；用 SCAN 逐批遍历（本工具的 keyspace 浏览器就是这么做的）", true
	case "DEBUG":
		return "DEBUG 是调试后门，可能在无提示的情况下破坏数据或让实例崩溃", true
	}
	return "", false
}

// 确保 Level 的类型可用（避免 import 未使用）。
var _ Level = R0
var _ = model.EngineRedis
