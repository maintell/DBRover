package drivertest

import (
	"context"
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
	found := false
	for _, o := range objs {
		if o.Name == name {
			found = true
			if o.Kind != model.KindTrigger {
				t.Errorf("Kind = %q，期望 trigger", o.Kind)
			}
		}
	}
	if !found {
		t.Fatalf("触发器 %s 没出现在列表里：%+v", name, objs)
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
	name, err := cfg.Sequence(ctx, s)
	_ = s.Close()
	if err != nil {
		t.Fatalf("建序列: %v", err)
	}

	objs, err := c.Meta().Objects(ctx, "", "", model.KindSequence)
	if err != nil {
		t.Fatalf("Objects(KindSequence): %v", err)
	}
	for _, o := range objs {
		if o.Name == name {
			if o.Kind != model.KindSequence {
				t.Errorf("Kind = %q，期望 sequence", o.Kind)
			}
			return
		}
	}
	t.Fatalf("序列 %s 没出现在列表里：%+v", name, objs)
}
