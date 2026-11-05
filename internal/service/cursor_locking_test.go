package service

import (
	"os"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/driver/dialect"
	"github.com/maintell/DBRover/internal/policy"
)

// TestRejectLockingStatements 守住「游标不接受会加行锁的语句」。
//
// SELECT ... FOR UPDATE 不写数据，所以分级里 Writes=false（这是对的），但它会加行锁；
// 游标是长连接，一条看起来是 SELECT 的语句能把行锁攥到用户关掉页面为止。这条用例钉住：
// 这类语句在游标这条路被拒，且提示指路到执行接口。
func TestRejectLockingStatements(t *testing.T) {
	pg := dialect.For("postgres")
	an, err := pg.Analyze("SELECT * FROM t WHERE id = 1 FOR UPDATE")
	if err != nil {
		t.Fatalf("分析: %v", err)
	}
	if !an.ForUpdate {
		t.Fatalf("方言应当识别出 FOR UPDATE，实际 %+v", an)
	}
	// 分级仍然说「不写数据」——这与拒绝并不矛盾：改的是接口契约，不是风险级别。
	if gr := policy.GradeStatement(pg, "SELECT * FROM t WHERE id = 1 FOR UPDATE", nil); gr.Writes {
		t.Errorf("FOR UPDATE 不该被算成写数据，实际 %+v", gr)
	}

	if err := rejectLocking([]prepared{{an: an}}); err == nil {
		t.Fatal("FOR UPDATE 应当被游标拒绝")
	} else if !strings.Contains(err.Error(), "执行接口") {
		t.Errorf("提示应指路到执行接口，实际 %q", err.Error())
	}

	// 光有用例证明「函数能拒」不够：还要证明它**被调用了**。
	src, rerr := os.ReadFile("cursor.go")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(src), "if err := rejectLocking(preps); err != nil {") {
		t.Error("rejectLocking 没在打开游标的路径上被调用——函数写着不用，测试照样绿")
	}

	plain, _ := pg.Analyze("SELECT * FROM t WHERE id = 1")
	if err := rejectLocking([]prepared{{an: plain}}); err != nil {
		t.Errorf("普通 SELECT 不该被拒：%v", err)
	}
}
