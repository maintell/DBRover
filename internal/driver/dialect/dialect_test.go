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
