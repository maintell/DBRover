package api

import (
	"errors"
	"testing"

	"github.com/maintell/DBRover/internal/driver"
)

// TestConnectFailureCodes 守住 FR-CON-03 的验收口径：
// 「每类失败有独立错误码与修复建议」。
//
// 分类本身由驱动给（它认识引擎错误码），这里只检查映射：每个类别都必须有一个
// 不重复的错误码和一句非空建议——两类共用一个码，用户就分不出是哪种失败了。
func TestConnectFailureCodes(t *testing.T) {
	kinds := []driver.ConnectErrorKind{
		driver.ConnectDNS,
		driver.ConnectRefused,
		driver.ConnectTimeout,
		driver.ConnectAuth,
		driver.ConnectNoDatabase,
		driver.ConnectTLS,
		driver.ConnectHandshake,
		driver.ConnectFile,
	}
	seen := map[string]driver.ConnectErrorKind{}
	for _, k := range kinds {
		code, hint := connectFailure(&driver.ConnectError{Kind: k, Err: errors.New("boom")})
		if code == "" || code == "DB_CONNECT_FAILED" {
			t.Errorf("%s 没有独立错误码（拿到 %q）", k, code)
		}
		if hint == "" {
			t.Errorf("%s 没有修复建议", k)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("%s 与 %s 共用了错误码 %s", k, prev, code)
		}
		seen[code] = k
	}

	// 没被分类的错误回落到通用码，且不给具体建议——编一句错误的建议比不给更糟。
	code, hint := connectFailure(errors.New("unclassified"))
	if code != "DB_CONNECT_FAILED" || hint != "" {
		t.Errorf("未分类错误应回落到通用码且无建议，实际 %q / %q", code, hint)
	}
}
