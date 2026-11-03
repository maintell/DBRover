package main

import (
	"os"
	"strings"
	"testing"
)

// withStdin 临时替换 os.Stdin，灌入指定内容。
func withStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		_ = r.Close()
	})
	go func() {
		_, _ = w.WriteString(input)
		_ = w.Close()
	}()
}

// TestReadSecretTrimsOnlyNewline 口令里的空格是有效字符，只该去掉行尾换行。
func TestReadSecretTrimsOnlyNewline(t *testing.T) {
	withStdin(t, "pa ss word\r\n")
	got, err := readSecretFromStdin()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got != "pa ss word" {
		t.Fatalf("口令被改了: %q", got)
	}
}

// TestReadSecretRejectsEmpty 空口令必须在存之前就拒掉。
//
// 存一个空串进凭据库，之后的表现是「配了但连不上」，比「没配」难查得多。
func TestReadSecretRejectsEmpty(t *testing.T) {
	for _, in := range []string{"", "\n", "\r\n", "   \n"} {
		withStdin(t, in)
		if _, err := readSecretFromStdin(); err == nil {
			t.Errorf("输入 %q 应被拒绝", in)
		}
	}
}

// TestSecretSubcommandsRejectUnknown 未知子命令要报错，不能静默什么都不做。
func TestSecretSubcommandsRejectUnknown(t *testing.T) {
	err := cmdSecret([]string{"nonsense", "-service", "dbrover-selftest/unknown"})
	if err == nil {
		t.Fatal("未知子命令应报错")
	}
	if !strings.Contains(err.Error(), "未知子命令") {
		t.Errorf("错误信息应说明原因: %v", err)
	}
}
