package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBrowserCommand 各平台的打开命令要稳定。
//
// 这条测试的意义在于「URL 不能经过 shell」：Windows 上换成 `cmd /c start`
// 时，URL 里的 & 会截断参数——这条断言把那种改法挡住。
func TestBrowserCommand(t *testing.T) {
	cases := []struct {
		goos string
		want string
		argc int
	}{
		{"windows", "rundll32", 2},
		{"darwin", "open", 1},
		{"linux", "xdg-open", 1},
	}
	const url = "http://127.0.0.1:7788/#t=abc&x=1"
	for _, c := range cases {
		name, args, err := browserCommand(c.goos, url)
		if err != nil {
			t.Fatalf("%s: %v", c.goos, err)
		}
		if name != c.want {
			t.Errorf("%s 的命令 = %q，期望 %q", c.goos, name, c.want)
		}
		if len(args) != c.argc {
			t.Errorf("%s 的参数个数 = %d，期望 %d", c.goos, len(args), c.argc)
		}
		if !strings.Contains(strings.Join(args, " "), url) {
			t.Errorf("%s 的参数里应原样带 URL: %v", c.goos, args)
		}
	}
}

// TestBrowserCommandUnknownPlatform 不认识的平台要报错，而不是猜一条命令。
func TestBrowserCommandUnknownPlatform(t *testing.T) {
	if _, _, err := browserCommand("plan9", "http://x"); err == nil {
		t.Fatal("未知平台应报错")
	}
}

// TestWaitForServerTimesOut 连不上时要在超时后返回，不能无限等。
func TestWaitForServerTimesOut(t *testing.T) {
	// 用一个必定连不上的地址（保留端口 1），超时设短一点。
	err := waitForServer("http://127.0.0.1:1", 300*1000*1000) // 300ms
	if err == nil {
		t.Fatal("连不上时应返回错误")
	}
}

// TestWaitForServerSucceeds 服务就绪后要立刻返回。
//
// 用 httptest 起一个真服务，而不是 fork 一个 dbrover——这条测的是「等待逻辑」，
// 不是「serve 能不能起」。
func TestWaitForServerSucceeds(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	start := time.Now()
	if err := waitForServer(ts.URL, 2*time.Second); err != nil {
		t.Fatalf("服务已就绪却报错: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("就绪后应立刻返回，实际等了 %v", elapsed)
	}
}
