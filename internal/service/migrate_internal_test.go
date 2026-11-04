package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

func col(name, raw string, kind model.TypeKind, maxLen, prec, scale int) model.Column {
	return model.Column{Name: name, Type: model.ColumnType{
		Raw: raw, Kind: kind, MaxLen: maxLen, Precision: prec, Scale: scale,
	}}
}

func table(cols ...model.Column) *model.TableDetail {
	return &model.TableDetail{Columns: cols}
}

// TestPlanMigration 用表驱动钉住 docs/08 §4.3 的规则。
//
// 重点不是「每种类型都写了」，而是三类边界不许糊过去：
// 收窄必须阻断、语义漂移必须 review、没规则不许假装 ok。
func TestPlanMigration(t *testing.T) {
	cases := []struct {
		name     string
		src      model.Column
		dst      *model.Column
		srcEng   model.Engine
		dstEng   model.Engine
		want     MigrateStatus
		contains string
	}{
		{
			name:   "同引擎同类型",
			src:    col("a", "varchar(32)", model.TypeString, 32, 0, 0),
			dst:    ptr(col("a", "varchar(32)", model.TypeString, 32, 0, 0)),
			srcEng: model.EngineMySQL, dstEng: model.EngineMySQL,
			want: MigrateOK,
		},
		{
			name:   "同引擎、目标表还不存在",
			src:    col("a", "TEXT", model.TypeString, 0, 0, 0),
			dst:    nil,
			srcEng: model.EngineSQLite,
			dstEng: model.EngineSQLite,
			want:   MigrateOK,
		},
		{
			name:   "长度收窄阻断",
			src:    col("a", "varchar(64)", model.TypeString, 64, 0, 0),
			dst:    ptr(col("a", "varchar(32)", model.TypeString, 32, 0, 0)),
			srcEng: model.EngineMySQL, dstEng: model.EngineMySQL,
			want: MigrateBlocked, contains: "超过目标上限",
		},
		{
			name:   "精度收窄阻断",
			src:    col("n", "numeric(20,6)", model.TypeDecimal, 0, 20, 6),
			dst:    ptr(col("n", "numeric(10,2)", model.TypeDecimal, 0, 10, 2)),
			srcEng: model.EnginePostgres, dstEng: model.EnginePostgres,
			want: MigrateBlocked,
		},
		{
			name:   "TINYINT(1) 要人确认",
			src:    col("flag", "tinyint(1)", model.TypeInteger, 0, 0, 0),
			srcEng: model.EngineMySQL, dstEng: model.EnginePostgres,
			want: MigrateReview, contains: "布尔",
		},
		{
			name:   "unsigned bigint",
			src:    col("id", "bigint unsigned", model.TypeInteger, 0, 0, 0),
			srcEng: model.EngineMySQL, dstEng: model.EnginePostgres,
			want: MigrateReview, contains: "溢出",
		},
		{
			name:   "ENUM",
			src:    col("state", "enum('a','b')", model.TypeString, 0, 0, 0),
			srcEng: model.EngineMySQL, dstEng: model.EnginePostgres,
			want: MigrateReview, contains: "CHECK",
		},
		{
			name:   "MySQL DATETIME 可直迁",
			src:    col("t", "datetime", model.TypeTimestamp, 0, 0, 0),
			srcEng: model.EngineMySQL, dstEng: model.EnginePostgres,
			want: MigrateOK,
		},
		{
			name:   "PG 数组阻断",
			src:    col("tags", "text[]", model.TypeString, 0, 0, 0),
			srcEng: model.EnginePostgres, dstEng: model.EngineMySQL,
			want: MigrateBlocked, contains: "数组",
		},
		{
			name:   "jsonb 到 MySQL",
			src:    col("doc", "jsonb", model.TypeJSON, 0, 0, 0),
			srcEng: model.EnginePostgres, dstEng: model.EngineMySQL,
			want: MigrateReview, contains: "GIN",
		},
		{
			name:   "Oracle DATE 含时间",
			src:    col("t", "date", model.TypeDate, 0, 0, 0),
			srcEng: model.EngineOracle, dstEng: model.EnginePostgres,
			want: MigrateReview, contains: "时分秒",
		},
		{
			name:   "Oracle NUMBER 无精度",
			src:    col("n", "number", model.TypeDecimal, 0, 0, 0),
			srcEng: model.EngineOracle, dstEng: model.EnginePostgres,
			want: MigrateReview, contains: "浮点",
		},
		{
			name:   "SQLite 一律走人工",
			src:    col("x", "ANY", model.TypeUnknown, 0, 0, 0),
			srcEng: model.EngineSQLite, dstEng: model.EnginePostgres,
			want: MigrateReview, contains: "亲和性",
		},
		{
			name:   "未知规则不假装安全",
			src:    col("x", "geography", model.TypeUnknown, 0, 0, 0),
			srcEng: model.EnginePostgres, dstEng: model.EngineMySQL,
			want: MigrateReview, contains: "人工确认",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var dst *model.TableDetail
			if c.dst != nil {
				dst = table(*c.dst)
			}
			plan := PlanMigration(table(c.src), dst, c.srcEng, c.dstEng)
			if len(plan.Columns) == 0 {
				t.Fatal("没有任何列结论")
			}
			got := plan.Columns[0]
			if got.Status != c.want {
				t.Fatalf("状态 = %s，期望 %s（理由：%s）", got.Status, c.want, got.Reason)
			}
			if c.contains != "" && !strings.Contains(got.Reason, c.contains) {
				t.Errorf("理由应包含 %q，实际 %q", c.contains, got.Reason)
			}
		})
	}
}

// TestPlanMigrationTargetOnlyColumn 目标多出的列要被指出（否则迁移后默认值坑人）。
func TestPlanMigrationTargetOnlyColumn(t *testing.T) {
	src := table(col("id", "int", model.TypeInteger, 0, 0, 0))
	dst := table(
		col("id", "int", model.TypeInteger, 0, 0, 0),
		col("created", "timestamp", model.TypeTimestamp, 0, 0, 0),
	)
	plan := PlanMigration(src, dst, model.EngineMySQL, model.EngineMySQL)
	if plan.Review == 0 {
		t.Fatal("目标多出的列应产生 review")
	}
	found := false
	for _, c := range plan.Columns {
		if c.Column == "created" && strings.Contains(c.Reason, "默认值") {
			found = true
		}
	}
	if !found {
		t.Fatalf("没有指出目标独有的列：%+v", plan.Columns)
	}
}

func ptr(c model.Column) *model.Column { return &c }

// TestBuildMigrationDDL 钉住三件事：ok 列自动用预检类型、review/blocked 列必须
// 有人工决策、决策里的类型要过白名单。
func TestBuildMigrationDDL(t *testing.T) {
	src := &model.TableDetail{Columns: []model.Column{
		{Name: "id", Type: model.ColumnType{Raw: "int", Kind: model.TypeInteger}, Nullable: false},
		{Name: "flag", Type: model.ColumnType{Raw: "tinyint(1)", Kind: model.TypeInteger}, Nullable: true},
	}}
	src.Columns[0].IsPrimary = true // id 是主键：建表语句必须带上它
	plan := PlanMigration(src, nil, model.EngineMySQL, model.EnginePostgres)

	// 只给了 id 的决策，flag 还缺：必须报错并点名。
	_, err := BuildMigrationDDL(model.EnginePostgres, "t", src, plan, map[string]string{"id": "integer"})
	if !errors.Is(err, ErrMigrateNeedsDecision) {
		t.Fatalf("缺决策应返回 ErrMigrateNeedsDecision，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "flag") {
		t.Errorf("错误里要点名 flag，实际 %q", err.Error())
	}

	// 补齐后应当渲染出目标端 DDL。
	ddlText, err := BuildMigrationDDL(model.EnginePostgres, "t", src, plan, map[string]string{
		"id": "integer", "flag": "boolean",
	})
	if err != nil {
		t.Fatalf("BuildMigrationDDL: %v", err)
	}
	if !strings.Contains(ddlText, `"id" integer NOT NULL`) {
		t.Errorf("DDL 应保留 NOT NULL 并引用标识符：\n%s", ddlText)
	}
	if !strings.Contains(ddlText, `"flag" boolean`) {
		t.Errorf("DDL 应使用人工决策的类型：\n%s", ddlText)
	}
	// 主键必须随建表一起建：没有主键的表迁过去既不能续跑也不能按行定位。
	if !strings.Contains(ddlText, `PRIMARY KEY ("id")`) {
		t.Errorf("DDL 应包含主键：\n%s", ddlText)
	}

	// 决策里的类型要过白名单：带分号的直接拒绝。
	_, err = BuildMigrationDDL(model.EnginePostgres, "t", src, plan, map[string]string{
		"id": "integer", "flag": "boolean; DROP TABLE x",
	})
	if err == nil || !strings.Contains(err.Error(), "不合法") {
		t.Fatalf("非法类型应被拒，实际 %v", err)
	}
}

// TestBuildMigrationIndexDDL 钉住两件事：普通索引要迁、约束背后的索引不许重复建。
func TestBuildMigrationIndexDDL(t *testing.T) {
	src := &model.TableDetail{
		Columns: []model.Column{
			{Name: "id", Type: model.ColumnType{Raw: "int", Kind: model.TypeInteger}, IsPrimary: true},
			{Name: "a", Type: model.ColumnType{Raw: "text", Kind: model.TypeString}},
			{Name: "b", Type: model.ColumnType{Raw: "text", Kind: model.TypeString}},
		},
		Indexes: []model.Index{
			{Name: "PRIMARY", Columns: []string{"id"}, Primary: true, Unique: true, Constraint: true},
			{Name: "uq_b", Columns: []string{"b"}, Unique: true, Constraint: true},
			{Name: "idx_a", Columns: []string{"a"}},
		},
	}
	stmts := BuildMigrationIndexDDL(model.EnginePostgres, "t", src)
	if len(stmts) != 1 {
		t.Fatalf("应只生成 1 条索引语句（约束索引跳过），实际 %v", stmts)
	}
	if !strings.Contains(stmts[0], `CREATE INDEX "idx_a" ON "t" ("a")`) {
		t.Errorf("索引语句不对：%s", stmts[0])
	}
}

// TestMigrationConstraintsAndDefaults 对应 ADR-0012 的四条验证。
func TestMigrationConstraintsAndDefaults(t *testing.T) {
	lit := "0"
	expr := "nextval('s')"
	src := &model.TableDetail{
		Columns: []model.Column{
			{Name: "id", Type: model.ColumnType{Raw: "int", Kind: model.TypeInteger}, IsPrimary: true},
			{Name: "email", Type: model.ColumnType{Raw: "text", Kind: model.TypeString}},
			{Name: "n", Type: model.ColumnType{Raw: "int", Kind: model.TypeInteger}, Default: &lit},
			{Name: "s", Type: model.ColumnType{Raw: "int", Kind: model.TypeInteger}, Default: &expr},
			{Name: "c", Type: model.ColumnType{Raw: "text", Kind: model.TypeString}, Comment: "有注释"},
		},
		Indexes: []model.Index{
			{Name: "uq_email", Columns: []string{"email"}, Unique: true, Constraint: true},
		},
		ForeignKeys: []model.ForeignKey{{Name: "fk_x", Columns: []string{"n"}, RefTable: "other", RefColumns: []string{"id"}}},
		Comment:     "表注释",
	}
	plan := PlanMigration(src, nil, model.EnginePostgres, model.EnginePostgres)
	ddlText, err := BuildMigrationDDL(model.EnginePostgres, "t", src, plan, nil)
	if err != nil {
		t.Fatalf("BuildMigrationDDL: %v", err)
	}

	// 1) 匿名唯一约束：有 UNIQUE，但不能出现源约束名。
	if !strings.Contains(ddlText, `UNIQUE ("email")`) {
		t.Errorf("缺少唯一约束：\n%s", ddlText)
	}
	if strings.Contains(ddlText, "uq_email") {
		t.Errorf("约束名不该沿用源名（PG 会撞索引名）：\n%s", ddlText)
	}
	// 2) 字面量默认值要搬。
	if !strings.Contains(ddlText, "DEFAULT 0") {
		t.Errorf("字面量默认值应当迁移：\n%s", ddlText)
	}
	// 3) 表达式默认值不搬。
	if strings.Contains(ddlText, "nextval") {
		t.Errorf("表达式默认值不该搬：\n%s", ddlText)
	}

	notes := strings.Join(BuildMigrationNotes(src), "\n")
	if !strings.Contains(notes, "列 s 的默认值未迁移") {
		t.Errorf("notes 要点名表达式默认值：%s", notes)
	}
	// 4) 注释与外键要出声。
	if !strings.Contains(notes, "列注释未迁移") || !strings.Contains(notes, "表注释未迁移") {
		t.Errorf("notes 要提到注释：%s", notes)
	}
	if !strings.Contains(notes, "外键未迁移") {
		t.Errorf("notes 要提到外键：%s", notes)
	}
}
