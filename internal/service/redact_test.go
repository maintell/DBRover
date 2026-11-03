package service_test

import (
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/service"
)

// TestRedactSQL 锁住「口令不进审计」这一条（docs/10 §6.2）。
//
// 各种引擎的写法都要覆盖：审计只该丢口令，不该丢账号名与动作——
// 否则「谁改了哪个账号的口令」就查不出来了。
func TestRedactSQL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// 结果里必须出现的片段（保留了上下文）
		keep []string
		// 结果里绝对不能出现的片段（口令）
		drop []string
	}{
		{
			name: "MySQL CREATE USER IDENTIFIED BY",
			in:   "CREATE USER 'app'@'%' IDENTIFIED BY 'p@ssw0rd!'",
			keep: []string{"CREATE USER", "IDENTIFIED BY", "app"},
			drop: []string{"p@ssw0rd!"},
		},
		{
			name: "MySQL ALTER USER IDENTIFIED BY",
			in:   "ALTER USER 'app'@'%' IDENTIFIED BY 'new-secret'",
			keep: []string{"ALTER USER", "***"},
			drop: []string{"new-secret"},
		},
		{
			name: "MySQL 旧写法 CREATE USER ... PASSWORD",
			in:   "CREATE USER app PASSWORD 'legacy-pass'",
			keep: []string{"CREATE USER", "***"},
			drop: []string{"legacy-pass"},
		},
		{
			name: "SET PASSWORD FOR",
			in:   "SET PASSWORD FOR 'app'@'%' = 'set-pass'",
			keep: []string{"SET PASSWORD FOR", "***"},
			drop: []string{"set-pass"},
		},
		{
			name: "PG ALTER ROLE PASSWORD",
			in:   "ALTER ROLE app PASSWORD 'pg-pass'",
			keep: []string{"ALTER ROLE"},
			drop: []string{"pg-pass"},
		},
		{
			name: "普通查询原样保留",
			in:   "SELECT * FROM users WHERE name = 'alice'",
			keep: []string{"SELECT * FROM users", "'alice'"},
		},
		{
			name: "连接串里的 password=",
			in:   "connect dsn host=db password='dsn-pass' port=5432",
			keep: []string{"host=db", "port=5432"},
			drop: []string{"dsn-pass"},
		},
	}

	for _, c := range cases {
		got := service.RedactSQL(c.in)
		for _, keep := range c.keep {
			if !strings.Contains(got, keep) {
				t.Errorf("%s：脱敏后丢了 %q（结果 %q）", c.name, keep, got)
			}
		}
		for _, drop := range c.drop {
			if strings.Contains(got, drop) {
				t.Errorf("%s：口令没被抹掉 %q（结果 %q）", c.name, drop, got)
			}
		}
	}
}

// TestRedactSQLKeepsNonPasswordLiterals 脱敏不能把普通字符串也吃掉。
func TestRedactSQLKeepsNonPasswordLiterals(t *testing.T) {
	in := "UPDATE users SET note = 'my password is fine' WHERE id = 1"
	got := service.RedactSQL(in)
	if !strings.Contains(got, "'my password is fine'") {
		t.Fatalf("普通字面量不该被动: %q", got)
	}
}
