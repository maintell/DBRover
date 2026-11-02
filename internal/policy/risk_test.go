package policy

import (
	"testing"

	"github.com/maintell/DBRover/internal/driver/dialect"
	"github.com/maintell/DBRover/internal/model"
)

func TestGradeStatement(t *testing.T) {
	rels := StaticRelations{
		"t":     {"id"},
		"order": {"id"},
	}

	cases := []struct {
		name   string
		engine model.Engine
		sql    string
		rel    Relations
		want   Level
		writes bool
	}{
		{"只读常量", model.EnginePostgres, "SELECT 1", nil, R0, false},
		{"SHOW TABLES", model.EngineMySQL, "SHOW TABLES", nil, R0, false},
		{"EXPLAIN 只读", model.EnginePostgres, "EXPLAIN SELECT 1", nil, R0, false},
		{"UPDATE 无 WHERE", model.EnginePostgres, "UPDATE t SET a=1", rels, R4, true},
		{"UPDATE 恒真 1=1", model.EnginePostgres, "UPDATE t SET a=1 WHERE 1=1", rels, R4, true},
		{"UPDATE 恒真 TRUE", model.EnginePostgres, "UPDATE t SET a=1 WHERE TRUE", rels, R4, true},
		{"UPDATE 命中主键", model.EnginePostgres, "UPDATE t SET a=1 WHERE id=5", rels, R1, true},
		{"UPDATE 限定名主键", model.EnginePostgres, "UPDATE t SET a=1 WHERE t.id=5", rels, R1, true},
		{"UPDATE 命中主键但无元数据", model.EnginePostgres, "UPDATE t SET a=1 WHERE id=5", nil, R2, true},
		{"UPDATE 非主键条件", model.EnginePostgres, "UPDATE t SET a=1 WHERE name='x'", rels, R2, true},
		{"UPDATE 主键 AND 非主键", model.EnginePostgres, "UPDATE t SET a=1 WHERE id=5 AND name='x'", rels, R1, true},
		{"DELETE 无 WHERE", model.EnginePostgres, "DELETE FROM t", rels, R4, true},
		{"DELETE 恒真", model.EnginePostgres, "DELETE FROM t WHERE 1=1", rels, R4, true},
		{"DELETE IN 主键不降级", model.EnginePostgres, "DELETE FROM t WHERE id IN (1,2)", rels, R2, true},
		// 回归：id=1 OR 1=1 语法上有等值，语义上是全表写，必须判 R4。
		{"UPDATE 恒真 OR", model.EnginePostgres, "UPDATE t SET a=1 WHERE id=1 OR 1=1", rels, R4, true},
		{"DELETE 恒真 OR", model.EnginePostgres, "DELETE FROM t WHERE 1=1 OR id=5", rels, R4, true},
		{"DELETE OR 非恒真", model.EnginePostgres, "DELETE FROM t WHERE id=5 OR name='x'", rels, R2, true},
		{"DELETE NOT 不得判主键", model.EnginePostgres, "DELETE FROM t WHERE NOT id=5", rels, R2, true},
		{"DELETE EXISTS 不误判恒真", model.EnginePostgres, "DELETE FROM t WHERE EXISTS (SELECT 1 FROM u WHERE u.id=t.id)", rels, R2, true},
		{"DELETE BETWEEN 非等值", model.EnginePostgres, "DELETE FROM t WHERE id BETWEEN 1 AND 9", rels, R2, true},
		{"DELETE 时间范围", model.EnginePostgres, "DELETE FROM t WHERE created_at < '2020-01-01'", rels, R2, true},
		{"INSERT 单行", model.EnginePostgres, "INSERT INTO t VALUES (1)", rels, R1, true},
		{"MERGE", model.EngineOracle, "MERGE INTO t USING s ON (t.id=s.id) WHEN MATCHED THEN UPDATE SET a=1", rels, R3, true},
		{"TRUNCATE", model.EngineMySQL, "TRUNCATE TABLE t", rels, R4, true},
		{"DROP TABLE", model.EnginePostgres, "DROP TABLE t", rels, R4, true},
		{"DROP INDEX", model.EnginePostgres, "DROP INDEX i", rels, R3, true},
		{"ALTER TABLE", model.EnginePostgres, "ALTER TABLE t ADD COLUMN c int", rels, R3, true},
		{"CREATE TABLE", model.EnginePostgres, "CREATE TABLE x (a int)", rels, R3, true},
		{"GRANT", model.EnginePostgres, "GRANT SELECT ON t TO u", rels, R3, true},
		{"BEGIN", model.EnginePostgres, "BEGIN", nil, R0, false},
		{"COMMIT", model.EnginePostgres, "COMMIT", nil, R0, false},
		{"SET 会话参数", model.EnginePostgres, "SET search_path TO x", nil, R0, false},
		{"CTE 内含写", model.EnginePostgres, "WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x", rels, R2, true},
		{"SELECT INTO", model.EngineMSSQL, "SELECT * INTO newtab FROM t", rels, R3, true},
		{"SELECT FOR UPDATE", model.EnginePostgres, "SELECT * FROM t FOR UPDATE", rels, R2, false},
		{"未知函数调用", model.EnginePostgres, "SELECT my_func() FROM t", rels, R0, false},
		{"CALL 无法识别", model.EnginePostgres, "CALL p()", rels, R4, true},
		{"EXEC 无法识别", model.EngineMSSQL, "EXEC sp_who", rels, R4, true},
		{"EXPLAIN ANALYZE 包写语句", model.EnginePostgres, "EXPLAIN ANALYZE DELETE FROM t", rels, R4, true},
		{"COPY FROM 写", model.EnginePostgres, "COPY t FROM STDIN", rels, R3, true},
		{"COPY TO 只读", model.EnginePostgres, "COPY t TO STDOUT", rels, R0, false},
		{"解析失败保守 R4", model.EnginePostgres, "SELECT 'abc", rels, R4, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := GradeStatement(dialect.For(c.engine), c.sql, c.rel)
			if g.Level != c.want {
				t.Errorf("Level = %s, 期望 %s（理由：%s）", g.Level, c.want, g.Reason)
			}
			if g.Writes != c.writes {
				t.Errorf("Writes = %v, 期望 %v", g.Writes, c.writes)
			}
			if g.Reason == "" {
				t.Error("Reason 不能为空")
			}
		})
	}
}

// TestReadOnlyGuardCoverage 断言只读连接会拦住的语句集合。
// 这是文档 FR-SEC / I1 的直接验收：任何可能写入的语句都必须 Writes=true。
func TestReadOnlyGuardCoverage(t *testing.T) {
	mustWrite := []string{
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET a=1",
		"DELETE FROM t",
		"CREATE TABLE t (a int)",
		"ALTER TABLE t ADD COLUMN c int",
		"DROP TABLE t",
		"TRUNCATE TABLE t",
		"GRANT SELECT ON t TO u",
		"WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x",
		"SELECT * INTO nt FROM t",
		"CALL p()",
		"COPY t FROM STDIN",
	}
	for _, sql := range mustWrite {
		g := GradeStatement(dialect.For(model.EnginePostgres), sql, nil)
		if !g.Writes {
			t.Errorf("%q 应被判定为写操作（%s）", sql, g.Reason)
		}
	}

	mustRead := []string{
		"SELECT 1",
		"SHOW TABLES",
		"EXPLAIN SELECT 1",
		"SELECT * FROM t FOR UPDATE",
	}
	for _, sql := range mustRead {
		g := GradeStatement(dialect.For(model.EnginePostgres), sql, nil)
		if g.Writes {
			t.Errorf("%q 不应被判定为写操作（%s）", sql, g.Reason)
		}
	}
}
