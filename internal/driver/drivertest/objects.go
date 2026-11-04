package drivertest

import (
	"context"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// testTriggerListing 验证「能力位说支持触发器」时，对象列表真的能列出来。
//
// 这条用例来自一次真实的错：MySQL / PG 声明 Triggers=true，但
// Objects(KindTrigger) 没有分支，调用安静地返回空列表——用户读到的不是
// 「这个工具还不会查」，而是「这张表没有触发器」。
func testTriggerListing(t *testing.T, cfg Config) {
	if cfg.Trigger == nil {
		t.Skip("这个引擎的测试环境没有提供建触发器的钩子")
	}
	ctx := context.Background()
	c, err := cfg.New(false)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = c.Close() }()

	if !c.Capabilities().Triggers {
		t.Fatal("能力位说不支持触发器，却提供了建触发器的钩子：两处必须一致")
	}

	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, "CREATE TABLE trig_probe (id INTEGER PRIMARY KEY, v VARCHAR(16))", nil); err != nil {
		_ = s.Close()
		t.Fatalf("建表: %v", err)
	}
	name, err := cfg.Trigger(ctx, s, "trig_probe")
	_ = s.Close()
	if err != nil {
		t.Fatalf("建触发器: %v", err)
	}

	objs, err := c.Meta().Objects(ctx, "", "", model.KindTrigger)
	if err != nil {
		t.Fatalf("Objects(KindTrigger): %v", err)
	}
	var ref model.ObjectRef
	for _, o := range objs {
		if o.Name == name {
			ref = o
			if o.Kind != model.KindTrigger {
				t.Errorf("Kind = %q，期望 trigger", o.Kind)
			}
		}
	}
	if ref.Name == "" {
		t.Fatalf("触发器 %s 没出现在列表里：%+v", name, objs)
	}

	// 只能看到名字、看不到它干什么，等于没做完：定义必须取得到。
	ddl, err := c.Meta().DDL(ctx, ref)
	if err != nil {
		t.Fatalf("DDL(%s): %v", name, err)
	}
	if !strings.Contains(strings.ToUpper(ddl), "TRIGGER") || !strings.Contains(ddl, name) {
		t.Fatalf("触发器定义不像 CREATE TRIGGER %s：%q", name, ddl)
	}

	// 按表过滤时不该把触发器混进来：两者是不同对象。
	tables, err := c.Meta().Objects(ctx, "", "", model.KindTable)
	if err != nil {
		t.Fatalf("Objects(KindTable): %v", err)
	}
	for _, o := range tables {
		if o.Name == name {
			t.Fatalf("按表过滤时不该出现触发器 %s", name)
		}
	}
}

// testSequenceListing 同理：序列是独立对象，声明支持就要列得出来。
func testSequenceListing(t *testing.T, cfg Config) {
	if cfg.Sequence == nil {
		t.Skip("这个引擎没有提供建序列的钩子")
	}
	ctx := context.Background()
	c, err := cfg.New(false)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = c.Close() }()

	if !c.Capabilities().Sequences {
		t.Fatal("能力位说不支持序列，却提供了建序列的钩子：两处必须一致")
	}

	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	name, expect, err := cfg.Sequence(ctx, s)
	if err != nil {
		t.Fatalf("建序列: %v", err)
	}

	objs, err := c.Meta().Objects(ctx, "", "", model.KindSequence)
	if err != nil {
		t.Fatalf("Objects(KindSequence): %v", err)
	}
	var ref model.ObjectRef
	for _, o := range objs {
		if o.Name == name {
			ref = o
		}
	}
	if ref.Name == "" {
		t.Fatalf("序列 %s 没出现在列表里：%+v", name, objs)
	}
	if ref.Kind != model.KindSequence {
		t.Errorf("Kind = %q，期望 sequence", ref.Kind)
	}

	// 取到的定义必须能拿来重建：删掉原序列，用 DDL 回放，再列一次必须还在。
	// 只检查字符串像不像，是自欺——拼错一个子句，引擎才会告诉你。
	ddl, err := c.Meta().DDL(ctx, ref)
	if err != nil {
		t.Fatalf("DDL(%s): %v", name, err)
	}
	if up := strings.ToUpper(ddl); !strings.Contains(up, "CREATE SEQUENCE") || !strings.Contains(ddl, name) {
		t.Fatalf("序列定义不像 CREATE SEQUENCE %s：%q", name, ddl)
	}
	// 期望片段来自建序列的那一方：少拼一个子句这里就会红。
	for _, want := range expect {
		if !strings.Contains(strings.ToUpper(ddl), strings.ToUpper(want)) {
			t.Fatalf("定义里缺 %q：%q", want, ddl)
		}
	}
	if cfg.DropSequence == nil {
		return
	}
	if err := cfg.DropSequence(ctx, s, name); err != nil {
		t.Fatalf("删序列: %v", err)
	}
	if _, err := s.Execute(ctx, strings.TrimSuffix(strings.TrimSpace(ddl), ";"), nil); err != nil {
		t.Fatalf("回放定义失败（拼出来的 DDL 不可用）：%v\n%s", err, ddl)
	}
	again, err := c.Meta().Objects(ctx, "", "", model.KindSequence)
	if err != nil {
		t.Fatalf("重建后 Objects: %v", err)
	}
	var rebuilt model.ObjectRef
	for _, o := range again {
		if o.Name == name {
			rebuilt = o
		}
	}
	if rebuilt.Name == "" {
		t.Fatalf("回放后序列 %s 不在列表里", name)
	}
	// 「能建出来」还不够：参数也得回到原样。否则重建出来的是另一个序列，
	// 只是恰好同名列。再取一次定义逐字比对。
	ddl2, err := c.Meta().DDL(ctx, rebuilt)
	if err != nil {
		t.Fatalf("回放后 DDL: %v", err)
	}
	if ddl2 != ddl {
		t.Fatalf("回放后的定义与原定义不一致（参数丢了）：\n原：%s\n后：%s", ddl, ddl2)
	}
}

// testSearchComments 验证 `meta.search` 的「注释」那一半真的有效。
//
// 只搜名字的话，用户按业务含义（「订单未支付」）找表永远找不到——注释就是
// 给这类问题准备的。
func testSearchComments(t *testing.T, cfg Config) {
	if cfg.Comment == nil {
		t.Skip("这个引擎没有表注释")
	}
	ctx := context.Background()
	c, err := cfg.New(false)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = c.Close() }()

	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	const table = "comment_probe"
	if _, err := s.Execute(ctx, "CREATE TABLE "+table+" (id INTEGER PRIMARY KEY)", nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	const token = "dbrover-comment-token-9f2c"
	if err := cfg.Comment(ctx, s, table, token); err != nil {
		t.Fatalf("写注释: %v", err)
	}

	// 用注释里的词搜：必须能找到这张表。
	objs, err := c.Meta().Search(ctx, "comment-token-9f2c", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, o := range objs {
		if o.Name == table {
			found = true
		}
	}
	if !found {
		t.Fatalf("按注释搜索没找到 %s，返回 %+v", table, objs)
	}

	// 反向：搜一个不存在于任何注释的词，不该把这张表带出来。
	objs, err = c.Meta().Search(ctx, "no-such-token-a71b", 20)
	if err != nil {
		t.Fatalf("Search(负向): %v", err)
	}
	for _, o := range objs {
		if o.Name == table {
			t.Fatalf("无关的词不该命中 %s", table)
		}
	}
}

// testDescribeReportsTableComment 验证表详情里能拿到**表**注释。
//
// 为什么单列一条：列注释一直有，表注释却长期缺着——MySQL 的
// information_schema.TABLES.TABLE_COMMENT、PG 的 obj_description 都查得到，
// 只是没接进 DescribeTable。缺了它，表详情看不到说明、迁移搬不过去、
// 比对只能比列注释，而表面上完全不像缺陷（列注释明明在）。
func testDescribeReportsTableComment(t *testing.T, cfg Config) {
	if cfg.Comment == nil {
		t.Skip("这个引擎没有表注释")
	}
	ctx := context.Background()
	c, err := cfg.New(false)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = c.Close() }()

	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	const table = "comment_detail_probe"
	if _, err := s.Execute(ctx, "CREATE TABLE "+table+" (id INTEGER PRIMARY KEY)", nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	const token = "表说明 token-3d51 中文"
	if err := cfg.Comment(ctx, s, table, token); err != nil {
		t.Fatalf("写注释: %v", err)
	}

	detail, err := c.Meta().DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: table})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if detail.Comment != token {
		t.Fatalf("表注释 = %q，期望 %q（说明 DescribeTable 没取表注释）", detail.Comment, token)
	}
}
