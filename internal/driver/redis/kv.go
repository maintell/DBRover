package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/maintell/DBRover/internal/driver"
)

// ErrReadOnly 表示连接是只读的，写操作被拒绝。
var ErrReadOnly = errors.New("该连接为只读，拒绝写命令")

// Scan 按游标遍历 key。
//
// 用 SCAN 而不是 KEYS：KEYS 会阻塞整个实例，集群模式下还没有它（docs/04 §5.6）。
func (c *Connector) Scan(ctx context.Context, opt driver.KVScanOptions) (*driver.KVPage, error) {
	cli, err := c.handle()
	if err != nil {
		return nil, err
	}
	count := int64(opt.Count)
	if count <= 0 {
		count = driver.KVDefaultScanCount
	}
	var cursor uint64
	if opt.Cursor != "" {
		v, err := strconv.ParseUint(opt.Cursor, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis: 游标不合法 %q", opt.Cursor)
		}
		cursor = v
	}

	var (
		keys    []string
		nextCur uint64
	)
	if opt.Type != "" && opt.Type != driver.KVType("") {
		keys, nextCur, err = cli.ScanType(ctx, cursor, opt.Pattern, count, string(opt.Type)).Result()
	} else {
		keys, nextCur, err = cli.Scan(ctx, cursor, opt.Pattern, count).Result()
	}
	if err != nil {
		return nil, fmt.Errorf("redis: SCAN 失败: %w", err)
	}

	page := &driver.KVPage{Keys: keys, HasMore: nextCur != 0}
	if page.HasMore {
		page.Cursor = strconv.FormatUint(nextCur, 10)
	}
	return page, nil
}

// Inspect 读一个 key 的类型、TTL、编码、元素数与体积。
func (c *Connector) Inspect(ctx context.Context, key string) (*driver.KVEntry, error) {
	cli, err := c.handle()
	if err != nil {
		return nil, err
	}
	typ, err := cli.Type(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("redis: TYPE 失败: %w", err)
	}
	entry := &driver.KVEntry{Key: key, Type: kvType(typ)}
	if entry.Type == driver.KVType("none") {
		// key 不存在：TTL 语义里 -2 表示不存在，如实返回而不是报错。
		entry.TTL = -2
		return entry, nil
	}

	ttl, err := cli.TTL(ctx, key).Result()
	if err == nil {
		entry.TTL = int64(ttl / time.Second)
		if ttl < 0 {
			entry.TTL = int64(ttl) // -1 永不过期；不要被上面的除法抹成 0
		}
	}
	if enc, err := cli.ObjectEncoding(ctx, key).Result(); err == nil {
		entry.Encoding = enc
	}

	// 元素数：按类型各问各的。取不到就不填（nil），不猜成 0。
	if n, ok := c.length(ctx, cli, key, entry.Type); ok {
		entry.Length = &n
	}

	// 体积：MEMORY USAGE 在大 key 上可能阻塞，超时就放弃（SizeBytes 留 nil）。
	mctx, cancel := context.WithTimeout(ctx, driver.KVInspectTimeout)
	defer cancel()
	if n, err := cli.MemoryUsage(mctx, key).Result(); err == nil {
		entry.SizeBytes = &n
	}

	// 预览：限长，避免一个 500MB 的 string 被整取。
	if entry.Type == driver.KVString {
		if s, err := cli.GetRange(ctx, key, 0, driver.KVDefaultPreviewBytes-1).Result(); err == nil {
			entry.Preview = s
		}
	}
	return entry, nil
}

// length 按类型取元素数。第二个返回值为 false 表示这种类型没有「元素数」概念。
func (c *Connector) length(ctx context.Context, cli *redis.Client, key string, typ driver.KVType) (int64, bool) {
	var (
		n   int64
		err error
	)
	switch typ {
	case driver.KVString:
		n, err = cli.StrLen(ctx, key).Result()
	case driver.KVHash:
		n, err = cli.HLen(ctx, key).Result()
	case driver.KVList:
		n, err = cli.LLen(ctx, key).Result()
	case driver.KVSet:
		n, err = cli.SCard(ctx, key).Result()
	case driver.KVZSet:
		n, err = cli.ZCard(ctx, key).Result()
	case driver.KVStream:
		n, err = cli.XLen(ctx, key).Result()
	default:
		return 0, false
	}
	if err != nil {
		return 0, false
	}
	return n, true
}

// Value 按类型分页读值。
func (c *Connector) Value(ctx context.Context, key string, opt driver.KVValueOptions) (*driver.KVValue, error) {
	cli, err := c.handle()
	if err != nil {
		return nil, err
	}
	typ, err := cli.Type(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("redis: TYPE 失败: %w", err)
	}
	t := kvType(typ)
	out := &driver.KVValue{Key: key, Type: t}
	if t == driver.KVType("none") {
		return out, nil
	}

	count := opt.Count
	if count <= 0 {
		count = 100
	}
	maxBytes := opt.MaxBytes
	if maxBytes <= 0 {
		maxBytes = driver.KVDefaultPreviewBytes
	}

	switch t {
	case driver.KVString:
		s, err := cli.GetRange(ctx, key, 0, int64(maxBytes)-1).Result()
		if err != nil {
			return nil, fmt.Errorf("redis: GETRANGE 失败: %w", err)
		}
		out.String = &s
		// 截断要显式给：读够上限与读尽是两件事。
		if n, err := cli.StrLen(ctx, key).Result(); err == nil && n > int64(maxBytes) {
			out.Truncated = true
		}

	case driver.KVHash:
		vals, next, err := cli.HScan(ctx, key, parseCursor(opt.Cursor), "", int64(count)).Result()
		if err != nil {
			return nil, fmt.Errorf("redis: HSCAN 失败: %w", err)
		}
		for i := 0; i+1 < len(vals); i += 2 {
			out.Pairs = append(out.Pairs, driver.KVPair{Field: vals[i], Value: vals[i+1]})
		}
		out.HasMore = next != 0
		if out.HasMore {
			out.Cursor = strconv.FormatUint(next, 10)
		}

	case driver.KVSet:
		vals, next, err := cli.SScan(ctx, key, parseCursor(opt.Cursor), "", int64(count)).Result()
		if err != nil {
			return nil, fmt.Errorf("redis: SSCAN 失败: %w", err)
		}
		for _, v := range vals {
			out.Pairs = append(out.Pairs, driver.KVPair{Member: v})
		}
		out.HasMore = next != 0
		if out.HasMore {
			out.Cursor = strconv.FormatUint(next, 10)
		}

	case driver.KVZSet:
		// 按名次分页：ZRANGE start stop 是闭区间，所以要 -1。
		start := int64(0)
		if opt.Cursor != "" {
			if v, err := strconv.ParseInt(opt.Cursor, 10, 64); err == nil {
				start = v
			}
		}
		zs, err := cli.ZRangeWithScores(ctx, key, start, start+int64(count)-1).Result()
		if err != nil {
			return nil, fmt.Errorf("redis: ZRANGE 失败: %w", err)
		}
		for _, z := range zs {
			member, _ := z.Member.(string)
			out.Pairs = append(out.Pairs, driver.KVPair{Member: member, Score: z.Score})
		}
		if total, err := cli.ZCard(ctx, key).Result(); err == nil && start+int64(len(zs)) < total {
			out.HasMore = true
			out.Cursor = strconv.FormatInt(start+int64(len(zs)), 10)
		}

	case driver.KVList:
		start := int64(0)
		if opt.Cursor != "" {
			if v, err := strconv.ParseInt(opt.Cursor, 10, 64); err == nil {
				start = v
			}
		}
		vals, err := cli.LRange(ctx, key, start, start+int64(count)-1).Result()
		if err != nil {
			return nil, fmt.Errorf("redis: LRANGE 失败: %w", err)
		}
		for i, v := range vals {
			out.Pairs = append(out.Pairs, driver.KVPair{Field: strconv.FormatInt(start+int64(i), 10), Value: v})
		}
		if total, err := cli.LLen(ctx, key).Result(); err == nil && start+int64(len(vals)) < total {
			out.HasMore = true
			out.Cursor = strconv.FormatInt(start+int64(len(vals)), 10)
		}

	case driver.KVStream:
		start := opt.Cursor
		if start == "" {
			start = "-"
		}
		msgs, err := cli.XRangeN(ctx, key, start, "+", int64(count)).Result()
		if err != nil {
			return nil, fmt.Errorf("redis: XRANGE 失败: %w", err)
		}
		for _, m := range msgs {
			parts := make([]string, 0, len(m.Values))
			for k, v := range m.Values {
				parts = append(parts, k+"="+fmt.Sprint(v))
			}
			out.Pairs = append(out.Pairs, driver.KVPair{ID: m.ID, Value: strings.Join(parts, " ")})
		}
		if len(msgs) == count {
			out.HasMore = true
			out.Cursor = "(" + msgs[len(msgs)-1].ID // XRANGE 的排他写法
		}

	default:
		// 位图 / HLL / geo / 模块类型：能看类型，不假装能编辑。
		out.Truncated = true
	}
	return out, nil
}

// Mutate 执行一次结构化写操作。
func (c *Connector) Mutate(ctx context.Context, op driver.KVMutation) (*driver.KVResult, error) {
	if !c.writable() {
		return nil, ErrReadOnly
	}
	cli, err := c.handle()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(op.Key) == "" {
		return nil, fmt.Errorf("redis: 缺少 key")
	}

	switch op.Kind {
	case driver.KVMutSetString:
		if op.TTL > 0 {
			err = cli.Set(ctx, op.Key, op.Value, time.Duration(op.TTL)*time.Second).Err()
		} else {
			err = cli.Set(ctx, op.Key, op.Value, 0).Err()
		}
	case driver.KVMutSetField:
		err = cli.HSet(ctx, op.Key, op.Field, op.Value).Err()
	case driver.KVMutDeleteKey:
		// UNLINK 优先：DEL 在大 key 上会阻塞主线程。
		var n int64
		n, err = cli.Unlink(ctx, op.Key).Result()
		if err == nil {
			return &driver.KVResult{Changed: n}, nil
		}
	case driver.KVMutDeleteItem:
		switch {
		case op.Field != "":
			var n int64
			n, err = cli.HDel(ctx, op.Key, op.Field).Result()
			if err == nil {
				return &driver.KVResult{Changed: n}, nil
			}
		case op.Member != "":
			var n int64
			n, err = cli.SRem(ctx, op.Key, op.Member).Result()
			if err == nil {
				return &driver.KVResult{Changed: n}, nil
			}
		default:
			return nil, fmt.Errorf("redis: 删除元素需要 field 或 member")
		}
	case driver.KVMutPush:
		var n int64
		n, err = cli.RPush(ctx, op.Key, op.Value).Result()
		if err == nil {
			return &driver.KVResult{Changed: n}, nil
		}
	case driver.KVMutExpire:
		var ok bool
		ok, err = cli.Expire(ctx, op.Key, time.Duration(op.TTL)*time.Second).Result()
		if err == nil {
			r := &driver.KVResult{Changed: 0}
			if ok {
				r.Changed = 1
			}
			return r, nil
		}
	default:
		return nil, fmt.Errorf("redis: 不支持的写操作 %q", op.Kind)
	}
	if err != nil {
		return nil, fmt.Errorf("redis: 写操作失败: %w", err)
	}
	// 写成功但引擎没给行数时，报 -1（未知），不假装是 0。
	return &driver.KVResult{Changed: -1}, nil
}

// Command 是命令模式的逃生口。
//
// **驱动不判断命令是读还是写**：它没有命令风险表，而按分层 `driver` 不能反向
// 依赖 `policy`。所以「这条命令能不能跑」由调用方（api 的命令风险分级）决定，
// 这里只负责执行。第一次实现时在驱动里加了「只读连接一律拒绝」，结果连
// `DBSIZE` 都被挡住——那等于把只读连接的命令模式整个关掉。
//
// 写操作仍然由 Mutate 的 writable() 守住，那里驱动确实知道自己在写。
func (c *Connector) Command(ctx context.Context, name string, args []any) (*driver.KVResult, error) {
	cli, err := c.handle()
	if err != nil {
		return nil, err
	}
	cmd := strings.TrimSpace(name)
	if cmd == "" {
		return nil, fmt.Errorf("redis: 缺少命令名")
	}
	argv := make([]any, 0, len(args)+1)
	argv = append(argv, cmd)
	argv = append(argv, args...)

	res, err := cli.Do(ctx, argv...).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("redis: 命令失败: %w", err)
	}
	return &driver.KVResult{Rows: [][]any{{res}}, Changed: -1}, nil
}

// kvType 把 Redis 的 TYPE 结果映射到中立类型。
func kvType(t string) driver.KVType {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "string":
		return driver.KVString
	case "hash":
		return driver.KVHash
	case "list":
		return driver.KVList
	case "set":
		return driver.KVSet
	case "zset":
		return driver.KVZSet
	case "stream":
		return driver.KVStream
	case "none":
		return driver.KVType("none")
	default:
		return driver.KVOther
	}
}

func parseCursor(s string) uint64 {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
