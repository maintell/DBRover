package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestDefaultFKName 验证自动生成的外键名是确定的、可复现的。
// 回滚要靠这个名字摘约束：随机名会让「撤销」无从下手，超长名必须截断而不是撞名。
func TestDefaultFKName(t *testing.T) {
	a := defaultFKName("child", []string{"pid"})
	b := defaultFKName("child", []string{"pid"})
	if a != b {
		t.Fatalf("同名同列应得到同一个名字: %q vs %q", a, b)
	}
	if a != "fk_child_pid" {
		t.Errorf("名字 = %q, 期望 fk_child_pid", a)
	}
	if got := defaultFKName("c", []string{"a", "b"}); got != "fk_c_a_b" {
		t.Errorf("多列名字 = %q", got)
	}

	long := strings.Repeat("t", 60)
	got := defaultFKName(long, []string{"c"})
	if len(got) > 64 {
		t.Errorf("超长名应被截断到 64 以内, 实际 %d 字符", len(got))
	}
	if got == defaultFKName(long, []string{"d"}) {
		t.Error("不同列不该截出同一个名字")
	}
}

// TestValidateFKShape 验证不碰数据库的那一半校验。
// 单独测它的理由：SQLite 不支持加外键，能力检查会把所有参数错误挡在同一句话上，
// 这些规则如果裹在能力检查后面，在没有真机的环境里就一条都测不到。
func TestValidateFKShape(t *testing.T) {
	child := &model.TableDetail{
		Ref: model.ObjectRef{Kind: model.KindTable, Name: "child"},
		Columns: []model.Column{
			{Name: "id", Type: model.ColumnType{Kind: model.TypeInteger}, Nullable: false, IsPrimary: true},
			{Name: "pid", Type: model.ColumnType{Kind: model.TypeInteger}, Nullable: true},
			{Name: "must", Type: model.ColumnType{Kind: model.TypeInteger}, Nullable: false},
		},
		ForeignKeys: []model.ForeignKey{{Name: "fk_exists", Columns: []string{"pid"}}},
	}
	ref := child.Ref

	ok := ForeignKeySpec{Columns: []string{"pid"}, RefTable: "parent", RefColumns: []string{"id"}}
	if got, err := validateFKShape(ref, child, ok); err != nil {
		t.Fatalf("合法请求不该报错: %v", err)
	} else if got.Name != "fk_child_pid" {
		t.Errorf("应生成确定的名字, 实际 %q", got.Name)
	}

	cases := []struct {
		name string
		fk   ForeignKeySpec
		want string
	}{
		{"缺父表", ForeignKeySpec{Columns: []string{"pid"}, RefColumns: []string{"id"}}, "父表"},
		{"列数为 0", ForeignKeySpec{RefTable: "parent"}, "至少要指定一列"},
		{"两侧列数不一致",
			ForeignKeySpec{Columns: []string{"pid"}, RefTable: "parent", RefColumns: []string{"id", "x"}}, "数量不一致"},
		{"子表没有该列",
			ForeignKeySpec{Columns: []string{"nope"}, RefTable: "parent", RefColumns: []string{"id"}}, "子表没有列"},
		{"动作不在白名单",
			ForeignKeySpec{Columns: []string{"pid"}, RefTable: "parent", RefColumns: []string{"id"}, OnDelete: "DROP TABLE"}, "ON DELETE"},
		{"SET NULL 撞上 NOT NULL",
			ForeignKeySpec{Columns: []string{"must"}, RefTable: "parent", RefColumns: []string{"id"}, OnDelete: "set null"}, "SET NULL"},
		{"外键名含引号",
			ForeignKeySpec{Name: `fk"; DROP TABLE t; --`, Columns: []string{"pid"}, RefTable: "parent", RefColumns: []string{"id"}}, "不合法"},
		{"外键已存在",
			ForeignKeySpec{Name: "fk_exists", Columns: []string{"pid"}, RefTable: "parent", RefColumns: []string{"id"}}, "已存在"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := validateFKShape(ref, child, c.fk)
			if err == nil {
				t.Fatal("应报错")
			}
			if !errors.Is(err, ErrInvalidDDL) {
				t.Errorf("应包裹 ErrInvalidDDL, 实际 %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应包含 %q, 实际 %v", c.want, err)
			}
		})
	}
}
