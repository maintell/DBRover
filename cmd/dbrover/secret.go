package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/maintell/DBRover/internal/infra/secret"
)

// cmdSecret 管理存放在系统凭据库里的口令。
//
// 为什么要单独一个命令，而不是让人把 key 写进环境变量或文件：
// docs/10 §2 要求「密码/私钥/口令只存 OS 凭据库」。环境变量会跟着进程
// 环境走（崩溃转储、子进程都看得到），明文文件更糟。
//
// 口令只从 **stdin** 读，绝不接受命令行参数：命令行会进进程列表与 shell 历史。
func cmdSecret(args []string) error {
	fs := flag.NewFlagSet("secret", flag.ExitOnError)
	service := fs.String("service", secret.AgentAPIKeyService, "凭据命名空间")
	account := fs.String("account", secret.AgentAPIKeyAccount, "凭据账号名")

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: dbrover secret (set|clear|status) [-service ...] [-account ...]")
		return errors.New("缺少子命令")
	}
	action := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	target := secret.Target(*service, *account)

	switch action {
	case "set":
		value, err := readSecretFromStdin()
		if err != nil {
			return err
		}
		if err := secret.Store(*service, *account, value); err != nil {
			if errors.Is(err, secret.ErrUnsupported) {
				return fmt.Errorf("当前平台不支持系统凭据库；不要把口令写进文件或环境变量")
			}
			return err
		}
		fmt.Fprintf(os.Stderr, "已保存到系统凭据库：%s\n", target)

	case "clear":
		if err := secret.Delete(*service, *account); err != nil {
			if errors.Is(err, secret.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "没有这条凭据：%s\n", target)
				return nil
			}
			return err
		}
		fmt.Fprintf(os.Stderr, "已删除：%s\n", target)

	case "status":
		// 只报「有没有」，绝不打印口令本身。
		if _, err := secret.Load(*service, *account); err != nil {
			switch {
			case errors.Is(err, secret.ErrNotFound):
				fmt.Fprintf(os.Stderr, "%s：未设置\n", target)
			case errors.Is(err, secret.ErrUnsupported):
				fmt.Fprintf(os.Stderr, "%s：当前平台不支持系统凭据库\n", target)
			default:
				return err
			}
			return nil
		}
		fmt.Fprintf(os.Stderr, "%s：已设置\n", target)

	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q；可用：set / clear / status\n", action)
		return errors.New("未知子命令")
	}
	return nil
}

// readSecretFromStdin 从标准输入读一行口令。
//
// 去行尾的换行与 \r，但保留中间的空格——口令里的空格是有效字符。
// 空输入直接拒绝：把空口令存进去，之后「配了但连不上」比「没配」更难查。
func readSecretFromStdin() (string, error) {
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("从 stdin 读口令失败: %w", err)
	}
	v := strings.TrimRight(line, "\r\n")
	// 判空用 TrimSpace：一串空格几乎一定是误操作（比如误按了回车又粘了空格），
	// 但存进去的值保持原样——口令里的前后空格如果是故意的，不能替人裁掉。
	if strings.TrimSpace(v) == "" {
		return "", errors.New("stdin 里没有内容；口令要按一行从标准输入传进来")
	}
	return v, nil
}
