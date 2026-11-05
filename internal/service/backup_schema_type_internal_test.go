package service

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestBackupSchemaOnlyType 锁住 -type schema（docs/08 §2.1）：段里只有建表与索引，
// 恢复出来是空表。
//
// 这是「结构备份」唯一值得断言的地方：它必须**真的没有数据**。只断言头里写着
// schema 是不够的——段内容还是 Export 的 sql 形态，漏传一个参数就会把全量数据
// 写进一个「只有结构」的文件里（体积与泄露面都翻倍）。
func TestBackupSchemaOnlyType(t *testing.T) {
	ctx := context.Background()
	src := openBackupTestDB(t)
	ex := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	var buf bytes.Buffer
	h, err := ex.Backup(ctx, BackupRequest{Type: BackupTypeSchema}, &buf)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if h.Type != BackupTypeSchema {
		t.Fatalf("头部类型 = %q，期望 %q", h.Type, BackupTypeSchema)
	}
	if !bytes.Contains(buf.Bytes(), []byte("CREATE TABLE")) {
		t.Fatal("结构备份里没有建表语句")
	}
	if bytes.Contains(buf.Bytes(), []byte("INSERT INTO")) {
		t.Fatal("结构备份里出现了 INSERT：数据没被排除，说明段还是按 full 生成的")
	}

	// 恢复到一个空库：表要在，行要为 0。
	dst := openEmptySQLite(t)
	dstEx := NewExecutor(dst, model.EnvTest, model.AccessReadWrite, nil)
	defer dstEx.Close()
	rep, err := dstEx.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreRequest{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rep.Rows != 0 {
		t.Errorf("结构备份恢复出 %d 行，期望 0", rep.Rows)
	}
	objs, err := dst.Meta().Objects(ctx, "", "", model.KindTable)
	if err != nil {
		t.Fatalf("列出目标对象: %v", err)
	}
	if len(objs) != len(rep.Objects) || len(objs) == 0 {
		t.Errorf("目标库有 %d 张表，报告说恢复了 %d 个对象", len(objs), len(rep.Objects))
	}
}

// TestBackupFullTypeIsDefault 是反向断言：不写 -type 时仍然是 full（带数据）。
//
// 少了这条，「默认值被改成 schema」这种改动没有任何用例会红，而它会让所有人的
// 日常备份静默失去数据。
func TestBackupFullTypeIsDefault(t *testing.T) {
	ctx := context.Background()
	src := openBackupTestDB(t)
	ex := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	var buf bytes.Buffer
	h, err := ex.Backup(ctx, BackupRequest{}, &buf)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if h.Type != BackupTypeFull {
		t.Fatalf("默认类型 = %q，期望 %q", h.Type, BackupTypeFull)
	}
	if !bytes.Contains(buf.Bytes(), []byte("INSERT INTO")) {
		t.Fatal("默认备份里没有 INSERT：默认类型不能是结构")
	}
}

// TestBackupRejectsUnsupportedType 三种输入：data 要给理由、乱写要拒绝、
// 大小写与空格要能容错（CLI 上写成 FULL 不该报错）。
func TestBackupRejectsUnsupportedType(t *testing.T) {
	ctx := context.Background()
	src := openBackupTestDB(t)
	ex := NewExecutor(src, model.EnvTest, model.AccessReadWrite, nil)
	defer ex.Close()

	var buf bytes.Buffer
	_, err := ex.Backup(ctx, BackupRequest{Type: BackupTypeData}, &buf)
	if err == nil {
		t.Fatal("data 类型不该被默默接受")
	}
	if !strings.Contains(err.Error(), "未实现") || !strings.Contains(err.Error(), "结构") {
		t.Errorf("data 的拒绝理由要说明缺什么，实际 %q", err.Error())
	}

	buf.Reset()
	_, err = ex.Backup(ctx, BackupRequest{Type: "whatever"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "只支持") {
		t.Fatalf("乱写的类型应被拒绝并说明支持哪些，实际 %v", err)
	}

	buf.Reset()
	if _, err := ex.Backup(ctx, BackupRequest{Type: " Schema "}, &buf); err != nil {
		t.Fatalf("大小写与空格应容错，实际 %v", err)
	}

	// normalizeBackupType 是唯一入口：此处直接钉住它的返回值。
	if got, err := normalizeBackupType("FULL"); err != nil || got != BackupTypeFull {
		t.Errorf("normalizeBackupType(FULL) = %q, %v", got, err)
	}
	if _, err := normalizeBackupType("data"); err == nil {
		t.Error("normalizeBackupType(data) 必须报错")
	}
}
