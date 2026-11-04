package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/service"
)

// TestDDLCreateTypeRejectedEngines 钉住「没有用户类型就别说能建」。
//
// MySQL 的 ENUM 是列类型、SQLite 根本没有这个概念；生成 CREATE TYPE 只会得到
// 一条必然失败的语句，而失败信息不会告诉用户「这个引擎用别的方式表达枚举」。
func TestDDLCreateTypeRejectedEngines(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	_, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
		Table: tableRef("t"), Action: service.DDLCreateType,
		Type: service.TypeSpec{Name: "ty_a", Values: []string{"a", "b"}},
	})
	if !errors.Is(err, service.ErrInvalidDDL) {
		t.Fatalf("SQLite 上建用户类型应被拒, 实际 %v", err)
	}
	if !strings.Contains(err.Error(), "ENUM 是列类型") {
		t.Errorf("要说明这个引擎用别的方式表达枚举：%v", err)
	}
}

// TestDDLCreateTypeRejectsBadInput 覆盖参数校验（与引擎无关的部分）。
//
// 这些错在哪台引擎上都不成立，所以必须比「引擎不支持」先报出来；而且都要能
// 指出是第几个值出的问题——引擎的报错只说「某个标签非法」。
func TestDDLCreateTypeRejectsBadInput(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ex := newTestExecutor(t, e)

	long := strings.Repeat("x", 64)
	cases := []struct {
		name string
		spec service.TypeSpec
		want string
	}{
		{"名字非法", service.TypeSpec{Name: "ty a", Values: []string{"a"}}, "字母、数字、下划线"},
		{"没有值", service.TypeSpec{Name: "ty_a"}, "至少要有一个值"},
		{"空值", service.TypeSpec{Name: "ty_a", Values: []string{"a", "  "}}, "枚举值不能为空"},
		{"值重复", service.TypeSpec{Name: "ty_a", Values: []string{"a", "b", "a"}}, "重复"},
		{"值太长", service.TypeSpec{Name: "ty_a", Values: []string{long}}, "超过 63 字节"},
		{"值含控制字符", service.TypeSpec{Name: "ty_a", Values: []string{"a\tb"}}, "控制字符"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("t"), Action: service.DDLCreateType, Type: c.spec,
			})
			if !errors.Is(err, service.ErrInvalidDDL) {
				t.Fatalf("应返回 ErrInvalidDDL, 实际 %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误应包含 %q，实际 %v", c.want, err)
			}
		})
	}
}

// TestDDLCreateTypeOnRealPG 验证建枚举的完整链路：语句带值、类型真能被列使用、
// 定义能还原、还被列用着时删不掉、回滚能删。
func TestDDLCreateTypeOnRealPG(t *testing.T) {
	for _, tgt := range rowsTargets(t) {
		tgt := tgt
		if tgt.name != "pg" {
			// 不能在这里 t.Skip：MySQL 排在前，跳过会终止整条用例，
			// pg 那一段根本没跑过（踩过一次）。
			continue
		}
		t.Run(tgt.name, func(t *testing.T) {
			ctx := context.Background()
			conn := tgt.open(t)
			ex := service.NewExecutor(conn, model.EnvTest, model.AccessReadWrite, nil)
			defer ex.Close()

			// 干净起点：上一轮中途失败可能留下表或类型。
			_ = execStrict(ctx, ex, "DROP TABLE IF EXISTS it_ty")
			_ = execStrict(ctx, ex, "DROP TYPE IF EXISTS it_state")

			plan, err := ex.BuildDDLPlan(ctx, service.DDLRequest{
				Table: tableRef("it_ty"), Action: service.DDLCreateType,
				Type: service.TypeSpec{Name: "it_state", Values: []string{"pending", "paid", "it's"}},
			})
			if err != nil {
				t.Fatalf("生成预案: %v", err)
			}
			if plan.Table.Kind != model.KindType {
				t.Fatalf("预案目标应是类型: %+v", plan.Table)
			}
			// 顺序必须与填写顺序一致（枚举的比较大小按声明顺序），引号要翻倍转义。
			if !strings.Contains(plan.Statements[0].SQL, "ENUM ('pending', 'paid', 'it''s')") {
				t.Fatalf("语句里的值与顺序不对：%s", plan.Statements[0].SQL)
			}
			if out, aerr := ex.ApplyChangePlan(ctx, plan, service.Request{Approval: true}); aerr != nil || out.Err != nil || out.Blocked {
				t.Fatalf("执行建类型: %v %+v", aerr, out)
			}

			// 建完必须能被列出来：这条断言让「对象树里看不到类型」在真机上红，
			// 而不是像只查「回滚后不存在」那样在列表功能坏掉时也能过。
			listed, lerr := conn.Meta().Objects(ctx, "", "", model.KindType)
			if lerr != nil {
				t.Fatalf("列类型: %v", lerr)
			}
			found := false
			for _, o := range listed {
				if o.Name == "it_state" {
					found = true
				}
			}
			if !found {
				t.Fatalf("刚建的类型没被列出来：%+v", listed)
			}

			// 真能用：建一张用它的表并写入，包括那个带单引号的值。
			if err := execStrict(ctx, ex, "CREATE TABLE it_ty (id int PRIMARY KEY, state it_state NOT NULL)"); err != nil {
				t.Fatalf("建表: %v", err)
			}
			if err := execStrict(ctx, ex, "INSERT INTO it_ty (id, state) VALUES (1, 'pending'), (2, 'paid'), (3, 'it''s')"); err != nil {
				t.Fatalf("写入: %v", err)
			}
			// 排序按声明顺序：pending 之前的没有，所以 state > 'pending' 命中两行；
			// 若按字母序（it's < paid < pending）会一行都不命中。
			if rows := queryRows(t, ctx, ex, "SELECT id FROM it_ty WHERE state > 'pending' ORDER BY id"); len(rows) != 2 {
				t.Fatalf("枚举顺序不对：state > 'pending' 命中 %d 行，应为 2（paid / it's）", len(rows))
			}

			// 定义能还原，且值顺序不变。
			ddlText, derr := ex.TableDDL(ctx, model.ObjectRef{Kind: model.KindType, Name: "it_state"})
			if derr != nil {
				t.Fatalf("取类型定义: %v", derr)
			}
			if !strings.Contains(ddlText, "AS ENUM ('pending', 'paid', 'it''s')") {
				t.Fatalf("还原出的定义不对：%s", ddlText)
			}

			// 还被列用着时必须删不掉：回滚语句不带 CASCADE，这是有意的。
			if err := execStrict(ctx, ex, "DROP TYPE it_state"); err == nil {
				t.Fatal("列还在用这个类型，DROP TYPE 应当失败")
			}

			if err := execStrict(ctx, ex, "DROP TABLE it_ty"); err != nil {
				t.Fatalf("删表: %v", err)
			}
			for _, s := range plan.Rollback {
				if err := execStrict(ctx, ex, s.SQL); err != nil {
					t.Fatalf("回滚（%s）: %v", s.SQL, err)
				}
			}
			objs, oerr := conn.Meta().Objects(ctx, "", "", model.KindType)
			if oerr != nil {
				t.Fatalf("列类型: %v", oerr)
			}
			for _, o := range objs {
				if o.Name == "it_state" {
					t.Fatal("回滚后类型应当不存在")
				}
			}
		})
	}
}

// queryRows 执行一条查询并返回首个结果集的行。执行期错误也算失败——
// 只判 err == nil 会把 execute 内部的失败当成成功。
func queryRows(t *testing.T, ctx context.Context, ex *service.Executor, sql string) [][]any {
	t.Helper()
	out, err := ex.Execute(ctx, service.Request{SQL: sql, Approval: true})
	if err != nil {
		t.Fatalf("执行 %q: %v", sql, err)
	}
	if out == nil || out.Blocked || out.Err != nil {
		t.Fatalf("%q 没跑通: %+v", sql, out)
	}
	if len(out.Results) == 0 {
		t.Fatalf("%q 没有结果集", sql)
	}
	return out.Results[0].Rows
}
