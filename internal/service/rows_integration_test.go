package service_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/mysql"
	"github.com/maintell/DBRover/internal/driver/pg"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// 筛选与排序要在真实引擎上验：SQLite 上过了不代表 MySQL / PostgreSQL 也过。
// 三处语义各不相同：
//   - 行构造器比较 (a, b) > (?, ?) 对 NULL 与类型推定的处理不同
//   - LIKE 的大小写与转义规则不同
//   - 整型列拿字符串去比，PG 会直接报错，SQLite 会静默按类型序比较
//
// 连接串形如：
//
//	DBRover_TEST_MYSQL=root:testpass@192.168.2.88:13306
//	DBRover_TEST_PG=postgres:testpass@192.168.2.88:15432
//
// 未设置时整组跳过，保证没有数据库的环境下 `go test ./...` 仍然全绿。
type rowsTarget struct {
	name    string
	user    string
	pass    string
	host    string
	port    int
	catalog string
	newConn func() driver.Connector
}

func parseConnString(t *testing.T, env string, raw string) (user, pass, host string, port int) {
	t.Helper()
	at := strings.LastIndex(raw, "@")
	if at <= 0 {
		t.Fatalf("%s 格式应为 user:pass@host:port，实际 %q", env, raw)
	}
	cred, addr := raw[:at], raw[at+1:]
	parts := strings.SplitN(cred, ":", 2)
	if len(parts) != 2 {
		t.Fatalf("%s 凭据部分应为 user:pass，实际 %q", env, cred)
	}
	i := strings.LastIndex(addr, ":")
	if i <= 0 || i == len(addr)-1 {
		t.Fatalf("%s 地址部分缺少端口: %q", env, addr)
	}
	p, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		t.Fatalf("%s 端口非法: %v", env, err)
	}
	return parts[0], parts[1], addr[:i], p
}

func rowsTargets(t *testing.T) []rowsTarget {
	t.Helper()
	var out []rowsTarget
	if raw := os.Getenv("DBRover_TEST_MYSQL"); raw != "" {
		u, p, h, port := parseConnString(t, "DBRover_TEST_MYSQL", raw)
		out = append(out, rowsTarget{
			name: "mysql", user: u, pass: p, host: h, port: port, catalog: "dbrover_test",
			newConn: func() driver.Connector { return mysql.New() },
		})
	}
	if raw := os.Getenv("DBRover_TEST_PG"); raw != "" {
		u, p, h, port := parseConnString(t, "DBRover_TEST_PG", raw)
		out = append(out, rowsTarget{
			name: "pg", user: u, pass: p, host: h, port: port, catalog: "dbrover_test",
			newConn: func() driver.Connector { return pg.New() },
		})
	}
	if len(out) == 0 {
		t.Skip("未设置 DBRover_TEST_MYSQL / DBRover_TEST_PG，跳过筛选与排序的引擎集成测试")
	}
	return out
}

func (tgt rowsTarget) open(t *testing.T) driver.Connector {
	t.Helper()
	conn := tgt.newConn()
	if err := conn.Open(context.Background(), driver.ConnectConfig{
		Host: tgt.host, Port: tgt.port,
		User: tgt.user, Password: tgt.pass,
		Catalog:     tgt.catalog,
		AccessMode:  model.AccessReadWrite,
		Environment: model.EnvTest,
	}); err != nil {
		t.Fatalf("连接 %s: %v", tgt.name, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// prepareTable 建表并写入 seed 行，同时登记「用独立连接删表」的清理动作。
// 清理单独开连接：被测执行器可能还持有会话，复用同一个会互相干扰。
func (tgt rowsTarget) prepareTable(t *testing.T, ddl []string, seed func(ctx context.Context, s driver.Session)) {
	t.Helper()
	ctx := context.Background()

	conn := tgt.open(t)
	s, err := conn.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()

	for _, q := range ddl {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("准备表失败 (%s): %v", q, err)
		}
	}
	if seed != nil {
		seed(ctx, s)
	}

	name := tableNameFromDDL(ddl[len(ddl)-1])
	t.Cleanup(func() {
		c := tgt.newConn()
		if err := c.Open(context.Background(), driver.ConnectConfig{
			Host: tgt.host, Port: tgt.port, User: tgt.user, Password: tgt.pass,
			Catalog: tgt.catalog, AccessMode: model.AccessReadWrite, Environment: model.EnvTest,
		}); err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		s2, err := c.Acquire(context.Background())
		if err != nil {
			return
		}
		_, _ = s2.Execute(context.Background(), "DROP TABLE IF EXISTS "+name, nil)
		_ = s2.Close()
	})
}

// tableNameFromDDL 从 "CREATE TABLE <name> (...)" 里取出表名。
func tableNameFromDDL(ddl string) string {
	fields := strings.Fields(ddl)
	for i, f := range fields {
		if strings.EqualFold(f, "TABLE") && i+1 < len(fields) {
			return strings.TrimSuffix(fields[i+1], "(")
		}
	}
	return "unknown"
}

// asInt64 容忍各引擎返回的整型包装差异（int64 / int32 / int）。
func asInt64(t *testing.T, v any) int64 {
	t.Helper()
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	default:
		t.Fatalf("非预期的整型返回类型 %T", v)
		return 0
	}
}

func TestTableRowsFilterAndSortOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP TABLE IF EXISTS it_rowprobe",
				"CREATE TABLE it_rowprobe (id INT PRIMARY KEY, v VARCHAR(32))",
			}, func(ctx context.Context, s driver.Session) {
				// 12 行只有 3 个不同的 v，必然出现重复排序键。
				for i := 1; i <= 12; i++ {
					v := []string{"aa", "bb", "cc"}[(i-1)%3]
					if _, err := s.Execute(ctx, "INSERT INTO it_rowprobe (id, v) VALUES (?, ?)", []any{i, v}); err != nil {
						t.Fatalf("插入 id=%d: %v", i, err)
					}
				}
			})

			ex := service.NewExecutor(tgt.open(t), model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			// 1) 整型列按数字比较：传字符串 "5" 必须等价于 id >= 5。
			res, err := ex.TableRows(ctx, service.TableRowsRequest{
				Table: "it_rowprobe", Limit: 100,
				Filter: []service.FilterCond{{Column: "id", Op: service.FilterGe, Value: "5"}},
			})
			if err != nil {
				t.Fatalf("按 id 筛选: %v", err)
			}
			if len(res.Rows) != 8 {
				t.Fatalf("id>=5 应为 8 行, 实际 %d", len(res.Rows))
			}
			if got := asInt64(t, res.Rows[0][0]); got != 5 {
				t.Errorf("首行 id = %d, 期望 5", got)
			}

			// 2) LIKE 只对文本生效。
			res, err = ex.TableRows(ctx, service.TableRowsRequest{
				Table: "it_rowprobe", Limit: 100,
				Filter: []service.FilterCond{{Column: "v", Op: service.FilterLike, Value: "aa%"}},
			})
			if err != nil {
				t.Fatalf("按 v 筛选: %v", err)
			}
			if len(res.Rows) != 4 {
				t.Errorf("v LIKE 'aa%%' 应为 4 行, 实际 %d", len(res.Rows))
			}

			// 3) 非唯一排序列的键集分页：每一行必须恰好出现一次。
			for _, desc := range []bool{false, true} {
				seen := map[int64]int{}
				var after []any
				for page := 0; ; page++ {
					res, err := ex.TableRows(ctx, service.TableRowsRequest{
						Table: "it_rowprobe", Limit: 5, After: after,
						Order: &service.OrderSpec{Column: "v", Desc: desc},
					})
					if err != nil {
						t.Fatalf("desc=%v 第 %d 页: %v", desc, page, err)
					}
					for _, r := range res.Rows {
						seen[asInt64(t, r[0])]++
					}
					if !res.HasMore {
						break
					}
					after = res.Next
					if page > 10 {
						t.Fatalf("desc=%v 翻页未终止", desc)
					}
				}
				if len(seen) != 12 {
					t.Fatalf("desc=%v 共翻到 %d 行, 期望 12", desc, len(seen))
				}
				for id, n := range seen {
					if n != 1 {
						t.Fatalf("desc=%v id=%d 出现 %d 次", desc, id, n)
					}
				}
			}

			// 4) 未知列必须报筛选错误，而不是被当成合法列拼进 SQL。
			if _, err := ex.TableRows(ctx, service.TableRowsRequest{
				Table: "it_rowprobe", Limit: 10,
				Filter: []service.FilterCond{{Column: "nope", Op: service.FilterEq, Value: "1"}},
			}); err == nil {
				t.Fatal("未知列应当报错")
			}
		})
	}
}

// TestTableRowsFilterNullOnRealEngines 验证 IS NULL / IS NOT NULL 在两引擎上一致，
// 并且「空串」不被当成 NULL。
func TestTableRowsFilterNullOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP TABLE IF EXISTS it_nullprobe",
				"CREATE TABLE it_nullprobe (id INT PRIMARY KEY, v VARCHAR(32) NULL)",
			}, func(ctx context.Context, s driver.Session) {
				for _, row := range []struct {
					id int
					v  any
				}{{1, "x"}, {2, nil}, {3, ""}} {
					if _, err := s.Execute(ctx, "INSERT INTO it_nullprobe (id, v) VALUES (?, ?)", []any{row.id, row.v}); err != nil {
						t.Fatalf("插入 id=%d: %v", row.id, err)
					}
				}
			})

			ex := service.NewExecutor(tgt.open(t), model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			res, err := ex.TableRows(ctx, service.TableRowsRequest{
				Table: "it_nullprobe", Limit: 10,
				Filter: []service.FilterCond{{Column: "v", Op: service.FilterIsNull}},
			})
			if err != nil {
				t.Fatalf("IS NULL: %v", err)
			}
			if len(res.Rows) != 1 {
				t.Fatalf("v IS NULL 应为 1 行, 实际 %d", len(res.Rows))
			}
			if got := asInt64(t, res.Rows[0][0]); got != 2 {
				t.Errorf("IS NULL 命中的 id = %d, 期望 2", got)
			}

			res, err = ex.TableRows(ctx, service.TableRowsRequest{
				Table: "it_nullprobe", Limit: 10,
				Filter: []service.FilterCond{{Column: "v", Op: service.FilterNotNull}},
			})
			if err != nil {
				t.Fatalf("IS NOT NULL: %v", err)
			}
			if len(res.Rows) != 2 {
				t.Errorf("v IS NOT NULL 应为 2 行（含空串）, 实际 %d", len(res.Rows))
			}
		})
	}
}

// TestTableRowsInAndBetweenOnRealEngines 验证 IN / BETWEEN 在两个真实引擎上的
// 形态：字符串型 IN 的值绑定要按列类型还原，BETWEEN 是闭区间。
//
// 单独一条用例而不是塞进上面那条：它验的是「值数组怎么绑」，与排序分页无关，
// 混在一起失败时看不出是哪一半坏了。
func TestTableRowsInAndBetweenOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP TABLE IF EXISTS it_inprobe",
				"CREATE TABLE it_inprobe (id INT PRIMARY KEY, v VARCHAR(32))",
			}, func(ctx context.Context, s driver.Session) {
				for i, v := range []string{"a", "b", "c", "a,b", "d", "b"} {
					if _, err := s.Execute(ctx, "INSERT INTO it_inprobe (id, v) VALUES (?, ?)", []any{i + 1, v}); err != nil {
						t.Fatalf("插入: %v", err)
					}
				}
			})

			ex := service.NewExecutor(tgt.open(t), model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			// IN：三个值，其中 "a,b" 是一个值而不是两个。
			res, err := ex.TableRows(ctx, service.TableRowsRequest{
				Table: "it_inprobe", Limit: 100,
				Filter: []service.FilterCond{{
					Column: "v", Op: service.FilterIn, Values: []string{"a", "c", "a,b"},
				}},
			})
			if err != nil {
				t.Fatalf("IN: %v", err)
			}
			if len(res.Rows) != 3 {
				t.Fatalf("IN 命中 %d 行, 期望 3（a、c、a,b 各一行）", len(res.Rows))
			}

			// BETWEEN：闭区间，1..3 应命中 3 行。
			res, err = ex.TableRows(ctx, service.TableRowsRequest{
				Table: "it_inprobe", Limit: 100,
				Filter: []service.FilterCond{{
					Column: "id", Op: service.FilterBetween, Values: []string{"1", "3"},
				}},
			})
			if err != nil {
				t.Fatalf("BETWEEN: %v", err)
			}
			if len(res.Rows) != 3 {
				t.Fatalf("BETWEEN 1..3 命中 %d 行, 期望 3", len(res.Rows))
			}

			// 边界：IN 空数组、BETWEEN 单值是调用方错误，必须报 ErrInvalidFilter
			// 而不是悄悄退化成「不过滤」。
			if _, err := ex.TableRows(ctx, service.TableRowsRequest{
				Table:  "it_inprobe",
				Filter: []service.FilterCond{{Column: "v", Op: service.FilterIn}},
			}); !errors.Is(err, service.ErrInvalidFilter) {
				t.Errorf("空 IN 应报 ErrInvalidFilter, 实际 %v", err)
			}
		})
	}
}
