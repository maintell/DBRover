package service_test

import (
	"bytes"
	"context"
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
