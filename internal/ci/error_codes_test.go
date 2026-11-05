package ci

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 错误码表里的行形状：竖线 + 反引号包住的 CODE + 竖线 + category...
//
// 只认大写字母、数字与下划线，且区分大小写：表里还有 engine_detail、request_id
// 这类字段名，宽松匹配会把它们当成错误码，然后报「代码里没有」的假警报。
// 反引号一律写成 \x60，避免与 Go 的 raw string 撞上。
// 形状必须完整（CODE + category + 三位状态码）：docs/12 里还有大量以反引号开头的
// 表行（HTTP 方法、路径），只认第一列会把 POST、PATCH 当成错误码。
var errCodeRowRe = regexp.MustCompile("(?m)^\\|\\s*\x60([A-Z][A-Z0-9_]+)\x60\\s*\\|\\s*[a-z_]+\\s*\\|\\s*[1-5][0-9][0-9]\\s*\\|")

// TestDocErrorCodesExistInCode 守住「错误码表里列的每一个码都真的会被产出」。
//
// 这条来自一次真实事故：审计发现 /query 会返回 EXEC_INVALID，而错误码表里根本
// 没有它——客户端按表写分支就会漏掉这一种。反过来同样成立：表里写着一个代码
// 从来不产出，调用方写了分支却永远进不去，比没有文档更浪费时间。
//
// 判定只做「代码里出现过这个字符串字面量」：产出位置可能在 api、service、driver
// 或 agent，逐个定位属于实现细节。但「一个都没有」一定是错的，而这正是要防的漂移。
func TestDocErrorCodesExistInCode(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "12-api-contract.md"))
	if err != nil {
		t.Fatalf("读 docs/12: %v", err)
	}
	codes := map[string]bool{}
	for _, m := range errCodeRowRe.FindAllStringSubmatch(string(doc), -1) {
		codes[m[1]] = true
	}
	if len(codes) < 20 {
		t.Fatalf("只从错误码表里认出 %d 个码，解析恐怕失效了", len(codes))
	}

	// 把 internal 与 cmd 下的非测试源码拼成一份 haystack：这些码只在服务端产出，
	// 前端自己拼的错误串不算「实现了这个码」。
	var b strings.Builder
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			b.Write(src)
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s: %v", dir, err)
		}
	}
	haystack := b.String()
	if len(haystack) < 100000 {
		t.Fatalf("只读到 %d 字节源码，遍历恐怕失效了", len(haystack))
	}

	var missing []string
	for code := range codes {
		if !strings.Contains(haystack, "\""+code+"\"") {
			missing = append(missing, code)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("错误码表里有 %d 个码在代码中从未产出：%s\n"+
			"要么把它实现掉，要么从表里删掉（表只列已实现的码；未实现的能力在正文里标注）",
			len(missing), strings.Join(missing, ", "))
	}
}
