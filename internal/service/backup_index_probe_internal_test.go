package service

import (
	"testing"

	"github.com/maintell/DBRover/internal/driver/dialect"
	"github.com/maintell/DBRover/internal/model"
)

// TestDDLIncludesIndexes 钉住「导出时要不要补 CREATE INDEX」的判断依据。
//
// 这个判断错一次就会出两种事故：说 true 而实际不含 → 备份恢复后索引全丢
// （唯一性不再被强制）；说 false 而实际含 → 恢复时撞名
// （PG 真机报过 relation already exists）。
func TestDDLIncludesIndexes(t *testing.T) {
	cases := map[model.Engine]bool{
		model.EngineSQLite:   false, // sqlite_master.sql 只存建表语句
		model.EngineMySQL:    true,  // SHOW CREATE TABLE 自带索引
		model.EnginePostgres: true,  // 驱动拼 DDL 时追加 pg_get_indexdef
	}
	for engine, want := range cases {
		if got := dialect.For(engine).DDLIncludesIndexes(); got != want {
			t.Errorf("%s: DDLIncludesIndexes = %v，期望 %v", engine, got, want)
		}
	}
}
