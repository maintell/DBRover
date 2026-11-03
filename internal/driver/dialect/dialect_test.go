package dialect

import (
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestIndexBuildAdvice 对应 docs/07 §2.4 / 验收 4：
// 加索引要给「怎么不锁表」的建议，且必须点明 CONCURRENTLY 不能在事务里跑——
// 否则用户照着做会得到一条必然失败的语句。
func TestIndexBuildAdvice(t *testing.T) {
	pgAdvice := For(model.EnginePostgres).IndexBuildAdvice("t", []string{"a", "b"}, false)
	for _, want := range []string{"CONCURRENTLY", "不能在事务里", "SQL 工作区"} {
		if !strings.Contains(pgAdvice, want) {
			t.Errorf("PG 建议缺少 %q：%s", want, pgAdvice)
		}
	}
	// 建议里要带上真实列名，而不是占位符。
	if !strings.Contains(pgAdvice, `"a"`) || !strings.Contains(pgAdvice, `"b"`) {
		t.Errorf("建议应包含引用后的列名：%s", pgAdvice)
	}

	myAdvice := For(model.EngineMySQL).IndexBuildAdvice("t", []string{"a"}, true)
	for _, want := range []string{"ALGORITHM=INPLACE", "LOCK=NONE"} {
		if !strings.Contains(myAdvice, want) {
			t.Errorf("MySQL 建议缺少 %q：%s", want, myAdvice)
		}
	}

	// SQLite 没有在线建索引的概念，不该硬凑一条建议。
	if got := For(model.EngineSQLite).IndexBuildAdvice("t", []string{"a"}, false); got != "" {
		t.Errorf("SQLite 不该给建议，实际 %q", got)
	}
}

// TestAlterTableClassifiedAsDDL 锁住手写 DDL 入口依赖的那条分类：
// 三个已交付引擎上，ALTER TABLE ... ADD COLUMN 都必须被判成 DDL。
//
// 手写 DDL 的第一道校验就是「必须是 DDL 类」；这条判错的后果很具体——
// 用户写的正常 ALTER 会被拒，或者 DML 被放进来绕过变更集。
func TestAlterTableClassifiedAsDDL(t *testing.T) {
	for _, e := range []model.Engine{model.EngineSQLite, model.EngineMySQL, model.EnginePostgres} {
		got, err := For(e).Classify("ALTER TABLE t ADD COLUMN c int")
		if err != nil {
			t.Errorf("%s: Classify 失败: %v", e, err)
			continue
		}
		if got != ClassDDL {
			t.Errorf("%s: ALTER TABLE 被判成 %s, 期望 ddl", e, got)
		}

		// 反面：DML 必须**不是** DDL，否则手写入口会放进本该走变更集的语句。
		if dml, err := For(e).Classify("UPDATE t SET c = 1"); err == nil && dml == ClassDDL {
			t.Errorf("%s: UPDATE 被判成 DDL", e)
		}
	}
}
