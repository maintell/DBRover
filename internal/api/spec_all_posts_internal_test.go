package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestSpecAllPostBodiesMatchService 一次性守住所有 POST 端点的请求体契约。
//
// 起因：/exec 补 stop_on_error 时漏了 spec；继续排查发现 agent/conversations、
// messages、explain 这些「代码里明明 decode(r, &req)」的端点，spec 里干脆没有
// requestBody——按契约生成的客户端根本不知道该 POST 什么。
//
// 做法是表驱动而不是给每个端点写一条测试：路径→请求结构体的映射是有限的、
// 可审阅的清单；字段一律从结构体的 json tag 用 reflect 取，绝不手抄（手抄的
// 清单会和 spec 一起漏，门禁就成了摆设）。两个方向都查：spec 少字段 = 客户端
// 不知道有这个能力；spec 多字段 = 真发过去被 DisallowUnknownFields 拒掉。
func TestSpecAllPostBodiesMatchService(t *testing.T) {
	// 路径 → 该端点 decode 进去的请求结构体。没有请求体的 POST 不在此列。
	bodies := []struct {
		path string
		typ  reflect.Type
	}{
		{"/api/v1/connections", reflect.TypeOf(createConnectionRequest{})},
		{"/api/v1/connections/{id}/exec", reflect.TypeOf(execRequest{})},
		{"/api/v1/connections/{id}/query", reflect.TypeOf(queryRequest{})},
		{"/api/v1/connections/{id}/explain", reflect.TypeOf(explainRequest{})},
		{"/api/v1/connections/{id}/changes/plan", reflect.TypeOf(changePlanRequest{})},
		{"/api/v1/connections/{id}/schema/plan", reflect.TypeOf(schemaPlanRequest{})},
		{"/api/v1/connections/{id}/bulk/plan", reflect.TypeOf(bulkPlanRequest{})},
		{"/api/v1/connections/{id}/fk/precheck", reflect.TypeOf(fkPrecheckRequest{})},
		{"/api/v1/connections/{id}/kv/apply", reflect.TypeOf(kvApplyRequest{})},
		{"/api/v1/compare", reflect.TypeOf(compareRequest{})},
		{"/api/v1/changes/{pid}/apply", reflect.TypeOf(changeApplyRequest{})},
		{"/api/v1/migrate/plan", reflect.TypeOf(migratePlanRequest{})},
		{"/api/v1/migrate/run", reflect.TypeOf(migrateRunRequest{})},
		{"/api/v1/agent/conversations", reflect.TypeOf(agentConvRequest{})},
		{"/api/v1/agent/conversations/{id}/messages", reflect.TypeOf(agentMessageRequest{})},
		{"/api/v1/agent/conversations/{id}/approvals/{pid}", reflect.TypeOf(agentApprovalRequest{})},
	}

	props := specAllPostProperties(t)
	for _, b := range bodies {
		want := map[string]bool{}
		for i := 0; i < b.typ.NumField(); i++ {
			tag := b.typ.Field(i).Tag.Get("json")
			if tag == "" || tag == "-" {
				continue
			}
			want[strings.Split(tag, ",")[0]] = true
		}
		got, exists := props[b.path]
		if !exists {
			t.Errorf("POST %s 代码里 decode(r,&%s)，但 openapi.json 没有 requestBody", b.path, b.typ.Name())
			continue
		}
		for f := range want {
			if !got[f] {
				t.Errorf("POST %s：入参 %q 在 %s 里有，openapi 的 requestBody 没声明", b.path, f, b.typ.Name())
			}
		}
		for f := range got {
			if !want[f] {
				t.Errorf("POST %s：openapi 声明了入参 %q，%s 里没有（真发过去会被拒成 unknown field）", b.path, f, b.typ.Name())
			}
		}
	}
}

// specAllPostProperties 取契约里所有 POST 端点的 requestBody.properties。
func specAllPostProperties(t *testing.T) map[string]map[string]bool {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(SpecJSON(), &doc); err != nil {
		t.Fatalf("openapi.json 不是合法 JSON: %v", err)
	}
	out := map[string]map[string]bool{}
	paths, _ := doc["paths"].(map[string]any)
	for path, item := range paths {
		ops, _ := item.(map[string]any)
		post, _ := ops["post"].(map[string]any)
		if post == nil {
			continue
		}
		body, _ := post["requestBody"].(map[string]any)
		content, _ := body["content"].(map[string]any)
		jsonBody, _ := content["application/json"].(map[string]any)
		schema, _ := jsonBody["schema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if len(props) == 0 {
			continue
		}
		set := map[string]bool{}
		for f := range props {
			set[f] = true
		}
		out[path] = set
	}
	return out
}
