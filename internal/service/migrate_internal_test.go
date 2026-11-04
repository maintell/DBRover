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
	// 4) 外键要说明是搬完后补建（注释由 BuildMigrationCommentDDL 单独处理）。
	if !strings.Contains(notes, "外键") || !strings.Contains(notes, "补建") {
		t.Errorf("notes 要说明外键是搬完后补建：%s", notes)
	}
}

// TestBuildMigrationForeignKeyDDL 对应 ADR-0013 的三条规则。
func TestBuildMigrationForeignKeyDDL(t *testing.T) {
	src := &model.TableDetail{
		Columns: []model.Column{{Name: "pid", Type: model.ColumnType{Raw: "int", Kind: model.TypeInteger}}},
		ForeignKeys: []model.ForeignKey{{
			Name: "fk_child_pid", Columns: []string{"pid"},
			RefTable: "parent", RefColumns: []string{"id"},
			OnDelete: "CASCADE",
		}},
	}

	// 1) 目标库有父表 → 建，并带动作子句。
	stmts, skipped := BuildMigrationForeignKeyDDL(model.EnginePostgres, true, "child", src,
		func(n string) bool { return n == "parent" })
	if len(stmts) != 1 || len(skipped) != 0 {
		t.Fatalf("应当生成 1 条语句：stmts=%v skipped=%v", stmts, skipped)
	}
	if !strings.Contains(stmts[0], "ON DELETE CASCADE") || !strings.Contains(stmts[0], `REFERENCES "parent"`) {
		t.Errorf("外键语句不完整：%s", stmts[0])
	}

	// 2) 目标库没有父表 → 跳过并点名。
	stmts, skipped = BuildMigrationForeignKeyDDL(model.EnginePostgres, true, "child", src,
		func(string) bool { return false })
	if len(stmts) != 0 {
		t.Fatalf("父表不存在时不该生成语句：%v", stmts)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "目标库没有表 parent") {
		t.Fatalf("跳过理由要点名父表：%v", skipped)
	}

	// 3) 引擎不支持加外键 → 全部跳过并说明。
	stmts, skipped = BuildMigrationForeignKeyDDL(model.EngineSQLite, false, "child", src,
		func(string) bool { return true })
	if len(stmts) != 0 {
		t.Fatalf("SQLite 不该生成外键语句：%v", stmts)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "不支持") {
		t.Fatalf("跳过理由要说清引擎不支持：%v", skipped)
	}
}

// TestBuildMigrationCommentDDL 钉住三种引擎的注释能力差异。
func TestBuildMigrationCommentDDL(t *testing.T) {
	src := &model.TableDetail{
		Comment: "表说明",
		Columns: []model.Column{
			{Name: "a", Comment: "列说明"},
			{Name: "b"},
		},
	}

	// PostgreSQL：表 + 列都能搬。
	stmts, skipped := BuildMigrationCommentDDL(model.EnginePostgres, "t", src)
	if len(stmts) != 2 || len(skipped) != 0 {
		t.Fatalf("PG 应生成 2 条注释语句：stmts=%v skipped=%v", stmts, skipped)
	}
	joined := strings.Join(stmts, "\n")
	if !strings.Contains(joined, `COMMENT ON TABLE "t"`) || !strings.Contains(joined, `COMMENT ON COLUMN "t"."a"`) {
		t.Errorf("注释语句不对：%s", joined)
	}
	if !strings.Contains(joined, "'表说明'") {
		t.Errorf("中文注释应原样写入：%s", joined)
	}

	// MySQL：只有表注释能一条语句搞定，列注释跳过并说明。
	stmts, skipped = BuildMigrationCommentDDL(model.EngineMySQL, "t", src)
	if len(stmts) != 1 || !strings.Contains(stmts[0], "COMMENT = ") {
		t.Fatalf("MySQL 应只生成表注释：%v", stmts)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "列注释未迁移") {
		t.Fatalf("跳过的列注释要说明原因：%v", skipped)
	}

	// SQLite：没有注释语法。
	stmts, skipped = BuildMigrationCommentDDL(model.EngineSQLite, "t", src)
	if len(stmts) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "没有注释语法") {
		t.Fatalf("SQLite 应全部跳过：stmts=%v skipped=%v", stmts, skipped)
	}
}

// TestQuoteLiteralByEngine 钉住「反斜杠只在 MySQL 里是转义符」这条引擎差异。
//
// 只双写单引号是半个转义：MySQL 默认把反斜杠当转义符，注释里的路径会被改写；
// PG 从 9.1 起 standard_conforming_strings=on，反斜杠就是普通字符，多写一个就
// 多存一个。两种错法都会静默改写用户写的注释，所以两边的期望值都要钉死。
func TestQuoteLiteralByEngine(t *testing.T) {
	const bs = "\\" // 反斜杠本身，省得在断言里数转义层数
	raw := "C:" + bs + "temp 与 '引号'"

	pg := quoteLiteral(model.EnginePostgres, raw)
	if want := "'C:" + bs + "temp 与 ''引号'''"; pg != want {
		t.Errorf("PG 不该动反斜杠：%q want %q", pg, want)
	}

	// MySQL：反斜杠双写，单引号仍然双写成 ''（MySQL 也接受这种写法）。
	my := quoteLiteral(model.EngineMySQL, raw)
	if want := "'C:" + bs + bs + "temp 与 ''引号'''"; my != want {
		t.Errorf("MySQL 的字面量不对：%q want %q", my, want)
	}
	if !strings.HasPrefix(my, "'") || !strings.HasSuffix(my, "'") {
		t.Errorf("字面量没有闭合：%q", my)
	}
}

// TestBuildMigrationCommentDDLTooLong 钉住「超长注释跳过并说明，不发一条注定 1629 的语句」。
func TestBuildMigrationCommentDDLTooLong(t *testing.T) {
	src := &model.TableDetail{Comment: strings.Repeat("注", mysqlMaxTableComment+1)}
	stmts, skipped := BuildMigrationCommentDDL(model.EngineMySQL, "t", src)
	if len(stmts) != 0 {
		t.Fatalf("超长注释不该生成语句：%v", stmts)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "超过 MySQL 上限") {
		t.Fatalf("跳过理由要说清超长：%v", skipped)
	}
	// 边界：正好等于上限要保留（差一错误的方向必须是「少拦」而不是「多拦」）。
	src.Comment = strings.Repeat("注", mysqlMaxTableComment)
	if stmts, _ := BuildMigrationCommentDDL(model.EngineMySQL, "t", src); len(stmts) != 1 {
		t.Fatalf("正好等于上限应保留：%v", stmts)
	}
}
