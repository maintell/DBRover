package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestSpecExecRequestMatchesService 守住 /exec 请求体的字段清单。
//
// 起因：给 /exec 加 stop_on_error（docs/05 §2.1）时，openapi.json 没同步——
// spec 只声明 sql / approval_token / max_rows，而 execRequest 还有 cursor 与
// stop_on_error。decode 用 DisallowUnknownFields，于是「按 spec 生成的客户端」
// 既不知道这两个能力，多传时又会被拒成 unknown field。
//
// 与 TestSpecSchemaPlanMatchesService 同一套办法：从结构体的 json tag 取真实
// 字段，两个方向都比对。不手抄清单——手抄的清单会和 spec 一起漏。
func TestSpecExecRequestMatchesService(t *testing.T) {
	props := specExecRequestProperties(t)

	want := map[string]bool{}
	typ := reflect.TypeOf(execRequest{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		want[strings.Split(tag, ",")[0]] = true
	}
	if len(want) < 4 {
		t.Fatalf("只从 execRequest 里认出 %d 个字段，解析恐怕失效了", len(want))
	}
	for f := range want {
		if _, ok := props[f]; !ok {
			t.Errorf("入参字段 %q 在 execRequest 里有，openapi 的 /exec requestBody 没声明", f)
		}
	}
	for f := range props {
		if !want[f] {
			t.Errorf("openapi 声明了 /exec 的入参 %q，execRequest 里没有（真发过去会被拒成 unknown field）", f)
		}
	}
}

// specExecRequestProperties 从契约里取 /exec 的 requestBody.properties。
func specExecRequestProperties(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(SpecJSON(), &doc); err != nil {
		t.Fatalf("openapi.json 不是合法 JSON: %v", err)
	}
	paths, _ := doc["paths"].(map[string]any)
	exec, _ := paths["/api/v1/connections/{id}/exec"].(map[string]any)
	post, _ := exec["post"].(map[string]any)
	body, _ := post["requestBody"].(map[string]any)
	content, _ := body["content"].(map[string]any)
	jsonBody, _ := content["application/json"].(map[string]any)
	schema, _ := jsonBody["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		t.Fatal("openapi.json 里取不到 /exec 的 requestBody.properties")
	}
	return props
}
