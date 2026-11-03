package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/maintell/DBRover/internal/driver"
)

// ErrNarrowingBlocked 表示目标类型的容量装不下现存数据。
//
// 这类失败必须在生成预案时就拦住：等到执行才发现，要么引擎报错（PG 会整体回滚，
// 还算安全），要么静默截断（某些配置下的 MySQL），后者是数据事故。
var ErrNarrowingBlocked = errors.New("目标类型的容量装不下现存数据")

// intTypeBounds 是整型的最小/最大值。范围取各引擎的通用有符号范围。
var intTypeBounds = map[string][2]int64{
	"tinyint":   {-128, 127},
	"int1":      {-128, 127},
	"smallint":  {-32768, 32767},
	"int2":      {-32768, 32767},
	"int":       {-2147483648, 2147483647},
	"integer":   {-2147483648, 2147483647},
	"int4":      {-2147483648, 2147483647},
	"mediumint": {-8388608, 8388607},
}

var (
	// varcharPattern 匹配 VARCHAR(64) / CHARACTER VARYING(64) / CHAR(8)
	lenTypePattern = regexp.MustCompile(`(?i)^(?:var)?char(?:acter)?(?:\s+varying)?\s*\(\s*([0-9]{1,6})\s*\)$`)
	// numericPattern 匹配 NUMERIC(10,2) / DECIMAL(10,2)
	numTypePattern = regexp.MustCompile(`(?i)^(?:numeric|decimal)\s*\(\s*([0-9]{1,6})\s*(?:,\s*([0-9]{1,6})\s*)?\)$`)
)

// narrowCheck 描述「新类型能否装下现有值」的探测条件。
type narrowCheck struct {
	// cond 是 WHERE 条件，col 已替换为引用后的列名。
	cond string
	// why 用于拼错误信息。
	why string
}

// boundsFor 依据新类型的原始声明给出探测条件。
// 返回 false 表示这个类型没有明确的容量上限（如 TEXT / BIGINT），无需探测。
func boundsFor(typeRaw, quotedCol string) (narrowCheck, bool) {
	t := strings.ToLower(strings.TrimSpace(typeRaw))

	if lo, ok := intTypeBounds[t]; ok {
		return narrowCheck{
			cond: fmt.Sprintf("(%s < %d OR %s > %d)", quotedCol, lo[0], quotedCol, lo[1]),
			why:  fmt.Sprintf("超出 %s 范围 [%d, %d]", typeRaw, lo[0], lo[1]),
		}, true
	}
	if m := lenTypePattern.FindStringSubmatch(t); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			return narrowCheck{}, false
		}
		return narrowCheck{
			cond: fmt.Sprintf("LENGTH(%s) > %d", quotedCol, n),
			why:  fmt.Sprintf("长度超过 %s 允许的 %d", typeRaw, n),
		}, true
	}
	if m := numTypePattern.FindStringSubmatch(t); m != nil {
		p, err1 := strconv.Atoi(m[1])
		s := 0
		if m[2] != "" {
			s, _ = strconv.Atoi(m[2])
		}
		if err1 != nil || p <= 0 || s < 0 || s > p {
			return narrowCheck{}, false
		}
		// 整数部分最多 p-s 位：绝对值必须小于 10^(p-s)。
		limit := int64(1)
		for i := 0; i < p-s; i++ {
			limit *= 10
		}
		return narrowCheck{
			cond: fmt.Sprintf("ABS(%s) >= %d", quotedCol, limit),
			why:  fmt.Sprintf("整数部分超过 %s 允许的 %d 位", typeRaw, p-s),
		}, true
	}
	return narrowCheck{}, false
}

// probeNarrowing 在生成预案前数一遍越界数据，并取几行样本。
// 查不出来时按「未知」处理：宁可放行让引擎报错，也不能因为探测失败就假装安全。
func (e *Executor) probeNarrowing(ctx context.Context, table, quotedCol, typeRaw string) (int64, []string, string, error) {
	check, ok := boundsFor(typeRaw, quotedCol)
	if !ok {
		return 0, nil, "", nil
	}

	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return 0, nil, "", err
	}
	defer func() { _ = sess.Close() }()

	where := " WHERE " + quotedCol + " IS NOT NULL AND " + check.cond

	cur, err := sess.Query(ctx, "SELECT COUNT(*) FROM "+table+where, nil, driver.QueryOptions{})
	if err != nil {
		return 0, nil, "", err
	}
	rows, _, err := cur.Next(ctx)
	_ = cur.Close()
	if err != nil || len(rows) != 1 || len(rows[0]) == 0 {
		return 0, nil, "", fmt.Errorf("统计越界数据失败")
	}
	bad, _ := toInt64(rows[0][0])
	if bad == 0 {
		return 0, nil, check.why, nil
	}

	samples := make([]string, 0, 3)
	scur, err := sess.Query(ctx, "SELECT "+quotedCol+" FROM "+table+where, nil, driver.QueryOptions{FetchSize: 3})
	if err == nil {
		srows, _, serr := scur.Next(ctx)
		_ = scur.Close()
		if serr == nil {
			for i, r := range srows {
				if i >= 3 || len(r) == 0 {
					break
				}
				samples = append(samples, fmt.Sprint(r[0]))
			}
		}
	}
	return bad, samples, check.why, nil
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int32:
		return int64(x), true
	case int:
		return int64(x), true
	case []byte:
		n, err := strconv.ParseInt(string(x), 10, 64)
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	}
	return 0, false
}
