package api_test

import (
	"net/http"
	"testing"
)

// TestMonitorEndpointsDegradeHonestly 钉住六个监控端点在「引擎没有这个能力」时的说法。
//
// 这条守的是文档里反复写的那句话：**能力没有就明确拒绝，不要给一个必然失败或必然
// 空的响应**。机器上没开留存、引擎没有计数器，用户看到的必须是一句能照做的说明，
// 而不是 200 + 一片空白（那会被读成「一切正常」）。
//
// 用 SQLite 连接跑：它没有服务端计数器、没有锁等待视图、没有体量表，正好是三个
// 降级分支的现实样本。
func TestMonitorEndpointsDegradeHonestly(t *testing.T) {
	_, ts, dir := newServer(t)
	connID := createConn(t, ts, dir, "test", "read-write")
	base := "/api/v1/connections/" + connID + "/monitor"

	// 三个「引擎没有」的端点：必须 400 + CAPABILITY_UNSUPPORTED，且话说清楚。
	for _, p := range []string{"/metrics", "/slow-queries", "/tables", "/locks"} {
		code, body := do(t, ts, http.MethodGet, base+p, nil, true)
		if code != http.StatusBadRequest {
			t.Errorf("%s 在没有这个能力的引擎上应返回 400，实际 %d：%v", p, code, body)
			continue
		}
		errObj, _ := body["error"].(map[string]any)
		if got, _ := errObj["code"].(string); got != "CAPABILITY_UNSUPPORTED" {
			t.Errorf("%s 的错误码应为 CAPABILITY_UNSUPPORTED，实际 %q", p, got)
		}
		if msg, _ := errObj["message"].(string); msg == "" {
			t.Errorf("%s 必须说明为什么不可用", p)
		}
	}

	// 历史：没开 -metrics 时 200 + available=false + 怎么开，而不是空数组。
	code, body := do(t, ts, http.MethodGet, base+"/history?metric=qps", nil, true)
	if code != http.StatusOK {
		t.Fatalf("history 应返回 200（没开留存不是错误），实际 %d：%v", code, body)
	}
	if avail, ok := body["available"].(bool); !ok || avail {
		t.Errorf("没开留存时 available 应为 false，实际 %v", body["available"])
	}
	if hint, _ := body["hint"].(string); hint == "" {
		t.Error("没开留存时必须告诉用户怎么开")
	}

	// 巡检与告警：拿不到数据也要出报告，且明确标 unknown / 无告警说明。
	code, body = do(t, ts, http.MethodGet, base+"/health-check", nil, true)
	if code != http.StatusOK {
		t.Fatalf("health-check 应返回 200，实际 %d：%v", code, body)
	}
	items, _ := body["items"].([]any)
	if len(items) == 0 {
		t.Fatal("巡检必须给出逐项结论，哪怕是 unknown")
	}

	code, body = do(t, ts, http.MethodGet, base+"/alerts", nil, true)
	if code != http.StatusOK {
		t.Fatalf("alerts 应返回 200，实际 %d：%v", code, body)
	}
	if note, _ := body["note"].(string); note == "" {
		t.Error("告警必须说明「只在应用运行时生效」")
	}
}
