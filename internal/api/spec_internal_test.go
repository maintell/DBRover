package api

import (
	"encoding/json"
	"strings"
	"testing"
)

type specDoc struct {
	OpenAPI string                                `json:"openapi"`
	Info    map[string]any                        `json:"info"`
	Paths   map[string]map[string]json.RawMessage `json:"paths"`
}

var httpMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true, "delete": true,
	"head": true, "options": true, "trace": true,
}

func loadSpec(t *testing.T) specDoc {
	t.Helper()
	var doc specDoc
	if err := json.Unmarshal(SpecJSON(), &doc); err != nil {
		t.Fatalf("契约不是合法 JSON: %v", err)
	}
	return doc
}

// TestSpecIsWellFormed 检查契约的基本结构，避免出现「字段忘了写」的低级问题。
func TestSpecIsWellFormed(t *testing.T) {
	doc := loadSpec(t)

	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Errorf("openapi = %q, 期望 3.x", doc.OpenAPI)
	}
	if doc.Info["title"] == nil || doc.Info["version"] == nil {
		t.Error("info.title 与 info.version 必填")
	}
	if len(doc.Paths) == 0 {
		t.Fatal("paths 为空")
	}

	for path, ops := range doc.Paths {
		if !strings.HasPrefix(path, "/api/v1/") {
			t.Errorf("路径 %q 不在 /api/v1/ 下", path)
		}
		for method, raw := range ops {
			if !httpMethods[method] {
				t.Errorf("%s %s: 未知方法", method, path)
				continue
			}
			var op struct {
				Summary   string                     `json:"summary"`
				Responses map[string]json.RawMessage `json:"responses"`
			}
			if err := json.Unmarshal(raw, &op); err != nil {
				t.Errorf("%s %s: 解析失败 %v", method, path, err)
				continue
			}
			if op.Summary == "" {
				t.Errorf("%s %s: 缺少 summary", method, path)
			}
			if len(op.Responses) == 0 {
				t.Errorf("%s %s: 缺少 responses", method, path)
			}
		}
	}
}

// TestSpecMatchesRoutes 是契约与实现的一致性门禁：
// 路由表里有的，契约必须有；契约里有的，路由表必须有。
func TestSpecMatchesRoutes(t *testing.T) {
	s := New("test-token", nil, nil)
	routes := s.routes()
	doc := loadSpec(t)

	specOps := map[string]bool{}
	for path, ops := range doc.Paths {
		for method := range ops {
			specOps[strings.ToUpper(method)+" "+path] = true
		}
	}

	routeOps := map[string]bool{}
	for _, rt := range routes {
		key := rt.Method + " " + rt.Path
		if routeOps[key] {
			t.Errorf("路由重复注册: %s", key)
		}
		routeOps[key] = true
	}

	for key := range routeOps {
		if !specOps[key] {
			t.Errorf("路由 %s 未在 openapi.json 中声明", key)
		}
	}
	for key := range specOps {
		if !routeOps[key] {
			t.Errorf("openapi.json 声明了不存在的路由 %s", key)
		}
	}
}

// TestSpecSecurityMatchesPublic 检查免鉴权标记与契约一致。
func TestSpecSecurityMatchesPublic(t *testing.T) {
	s := New("test-token", nil, nil)
	doc := loadSpec(t)

	byKey := map[string]json.RawMessage{}
	for path, ops := range doc.Paths {
		for method, raw := range ops {
			byKey[strings.ToUpper(method)+" "+path] = raw
		}
	}

	for _, rt := range s.routes() {
		key := rt.Method + " " + rt.Path
		raw, ok := byKey[key]
		if !ok {
			continue // 由 TestSpecMatchesRoutes 报告
		}
		var op map[string]json.RawMessage
		if err := json.Unmarshal(raw, &op); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		sec, hasSec := op["security"]
		if rt.Public {
			if !hasSec {
				t.Errorf("%s 是公开端点，契约应显式写 \"security\": []", key)
				continue
			}
			if strings.TrimSpace(string(sec)) != "[]" {
				t.Errorf("%s 是公开端点，security 应为 []，实际 %s", key, sec)
			}
		} else if hasSec {
			t.Errorf("%s 需要鉴权，契约不应覆盖全局 security", key)
		}
	}
}
