package service_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/dialect"
	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestSQLExportRoundTrip 是导出 sql 格式的端到端证据：
// 导出的脚本要能被**同一引擎**重新执行，并把数据原样建回来。
//
// 只验同引擎：字面量是按方言写的（二进制 X'..' 与 '\x..'::bytea、布尔 1/0 与
// TRUE/FALSE），跨引擎回放需要先做类型映射，不在这个格式的承诺范围内。
func TestSQLExportRoundTrip(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	ex := service.NewExecutor(e.rw, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	s, err := e.rw.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	for _, q := range []string{
		"DROP TABLE IF EXISTS exp_sql",
		"CREATE TABLE exp_sql (id INTEGER PRIMARY KEY, s TEXT, n REAL, b BLOB)",
	} {
		if _, err := s.Execute(ctx, q, nil); err != nil {
			t.Fatalf("准备表: %v", err)
		}
	}
	rows := []struct {
		id int
		s  any
		n  any
		b  any
	}{
		{1, "it's fine", 1.5, []byte{0x00, 0xff}},
		{2, "", nil, nil},
		{3, nil, -2.25, []byte("ok")},
	}
	for _, r := range rows {
		if _, err := s.Execute(ctx,
			"INSERT INTO exp_sql (id, s, n, b) VALUES (?, ?, ?, ?)",
			[]any{r.id, r.s, r.n, r.b}); err != nil {
			t.Fatalf("插入: %v", err)
		}
	}

	var buf bytes.Buffer
	res, err := ex.Export(ctx, service.ExportRequest{Table: "exp_sql", Format: service.ExportSQL}, &buf)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.Rows != 3 {
		t.Fatalf("导出 %d 行，期望 3", res.Rows)
	}
	script := buf.String()
	if !strings.Contains(script, "CREATE TABLE") || !strings.Contains(script, "INSERT INTO") {
		t.Fatalf("脚本里应当既有建表也有插入：\n%s", script)
	}
	if !strings.Contains(script, "it''s fine") {
		t.Errorf("单引号应双写转义：\n%s", script)
	}

	// 回放：删表 → 跑脚本 → 数据必须一致。
	if _, err := s.Execute(ctx, "DROP TABLE exp_sql", nil); err != nil {
		t.Fatalf("删表: %v", err)
	}
	stmts, err := dialect.For(model.EngineSQLite).Split(script)
	if err != nil {
		t.Fatalf("切分导出的脚本: %v", err)
	}
	for _, st := range stmts {
		// 脚本开头的注释会和第一条语句落在同一段里：去掉注释行再执行，
		// 但不要整段跳过——那样连建表语句一起丢了。
		var keep []string
		for _, line := range strings.Split(st.Text, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				keep = append(keep, line)
			}
		}
		exec := strings.TrimSpace(strings.Join(keep, "\n"))
		if exec == "" {
			continue
		}
		if _, err := s.Execute(ctx, exec, nil); err != nil {
			t.Fatalf("回放失败（%s）：%v", exec, err)
		}
	}

	cur, err := s.Query(ctx, "SELECT id, s, n, b FROM exp_sql ORDER BY id", nil, driver.QueryOptions{MaxRows: 10})
	if err != nil {
		t.Fatalf("回读: %v", err)
	}
	defer func() { _ = cur.Close() }()
	got, _, err := cur.Next(ctx)
	if err != nil {
		t.Fatalf("回读: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("回放后 %d 行，期望 3", len(got))
	}
	if got[0][1] != "it's fine" {
		t.Errorf("第 1 行 s = %v", got[0][1])
	}
	if got[1][2] != nil {
		t.Errorf("第 2 行 n 应为 NULL，实际 %v", got[1][2])
	}
}

// TestSQLExportRoundTripOnRealEngines 把 sql 导出的往返放到真机上：
// MySQL 5.7 与 PG 17 各自导出、删表、用脚本重建。
//
// 字段只用两引擎都接受的类型；二进制字面量的差异由 TestSQLLiteral 逐条断言，
// 不在这里硬凑一套跨引擎 DDL。
func TestSQLExportRoundTripOnRealEngines(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			tgt.prepareTable(t, []string{
				"DROP TABLE IF EXISTS exp_rt",
				"CREATE TABLE exp_rt (id INT PRIMARY KEY, s VARCHAR(32), n NUMERIC(10,2))",
			}, func(ctx context.Context, sess driver.Session) {
				for _, row := range [][]any{
					{1, "it's", "1.50"},
					{2, nil, nil},
					{3, "plain", "-2.25"},
				} {
					if _, err := sess.Execute(ctx,
						"INSERT INTO exp_rt (id, s, n) VALUES (?, ?, ?)", row); err != nil {
						t.Fatalf("插入: %v", err)
					}
				}
			})

			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			var buf bytes.Buffer
			if _, err := ex.Export(ctx, service.ExportRequest{
				Table: "exp_rt", Format: service.ExportSQL,
			}, &buf); err != nil {
				t.Fatalf("Export: %v", err)
			}
			script := buf.String()
			if !strings.Contains(script, "it''s") {
				t.Fatalf("单引号没有双写：\n%s", script)
			}

			s, err := conn.Acquire(ctx)
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			defer func() { _ = s.Close() }()
			if _, err := s.Execute(ctx, "DROP TABLE exp_rt", nil); err != nil {
				t.Fatalf("删表: %v", err)
			}

			stmts, err := dialect.For(conn.Engine()).Split(script)
			if err != nil {
				t.Fatalf("切分脚本: %v", err)
			}
			for _, st := range stmts {
				var keep []string
				for _, line := range strings.Split(st.Text, "\n") {
					if !strings.HasPrefix(strings.TrimSpace(line), "--") {
						keep = append(keep, line)
					}
				}
				exec := strings.TrimSpace(strings.Join(keep, "\n"))
				if exec == "" {
					continue
				}
				if _, err := s.Execute(ctx, exec, nil); err != nil {
					t.Fatalf("回放失败（%s）：%v", exec, err)
				}
			}

			// 用聚合代替逐值比对：NUMERIC 在不同驱动里可能是字符串也可能是字节，
			// 而「行数 + 非空数」能同时证明数据到齐、NULL 没被写成空串。
			cur, err := s.Query(ctx,
				"SELECT COUNT(*), COUNT(s), COUNT(n), SUM(id) FROM exp_rt", nil,
				driver.QueryOptions{MaxRows: 1})
			if err != nil {
				t.Fatalf("回读: %v", err)
			}
			defer func() { _ = cur.Close() }()
			rows, _, err := cur.Next(ctx)
			if err != nil || len(rows) != 1 {
				t.Fatalf("回读: err=%v rows=%v", err, rows)
			}
			got := rows[0]
			// id=1 与 id=3 有 n，id=2 的 s 与 n 都是 NULL。
			want := []string{"3", "2", "2", "6"}
			for i := range want {
				if sqlAggText(got[i]) != want[i] {
					t.Errorf("聚合第 %d 列 = %v，期望 %s", i+1, got[i], want[i])
				}
			}
		})
	}
}

// sqlAggText 把聚合结果转成可比对的文本（各驱动返回类型不一致）。
func sqlAggText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case string:
		return x
	case int64:
		return fmt.Sprint(x)
	default:
		return fmt.Sprint(x)
	}
}
