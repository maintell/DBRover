package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestSpecSchemaPlanMatchesService 守住 /schema/plan 的动作枚举与入参字段。
//
// 这两样都曾经落后于服务层：`action` 的 enum 只列 9 个（服务层 19 个），请求体只
// 声明 6 个字段（`schemaPlanRequest` 12 个）。它不会让任何用例变红，后果却是实实在在
// 的——按 spec 生成的客户端会把合法动作判成非法，读文档的人也无从知道「加约束要传
// 什么」。路由有 TestSpecMatchesRoutes 管，动作与字段此前没人管。
//
// 判据取服务层的常量定义（解析 ddl.go 的 AST）与请求结构的 json 标签，**不从枚举
// 自证**：两边都从代码里读，任何一侧增删而另一侧没跟上就会红。
func TestSpecSchemaPlanMatchesService(t *testing.T) {
	wantActions := serviceDDLActions(t)
	props := specSchemaPlanProperties(t)

	gotActions := map[string]bool{}
	action, ok := props["action"].(map[string]any)
	if !ok {
		t.Fatalf("契约里 /schema/plan 的 action 不是对象：%v", props["action"])
	}
	enum, ok := action["enum"].([]any)
	if !ok || len(enum) == 0 {
		t.Fatalf("契约里 /schema/plan 的 action 没有 enum：%v", action)
	}
	for _, v := range enum {
		s, _ := v.(string)
		gotActions[s] = true
	}

	for a := range wantActions {
		if !gotActions[a] {
			t.Errorf("动作 %q 服务层支持，openapi 的 enum 里没有", a)
		}
	}
	for a := range gotActions {
		if !wantActions[a] {
			t.Errorf("openapi 的 enum 里有 %q，服务层不认识（请求会被 ErrInvalidDDL 拒掉）", a)
		}
	}

	// 入参字段两个方向都查：spec 多一个，真发过去会被 DisallowUnknownFields 拒成
	// unknown field；少一个，文档就没有告诉用户该传哪个字段。
	wantFields := map[string]bool{}
	typ := reflect.TypeOf(schemaPlanRequest{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		wantFields[strings.Split(tag, ",")[0]] = true
	}
	if len(wantFields) < 10 {
		t.Fatalf("只从 schemaPlanRequest 里认出 %d 个字段，解析恐怕失效了", len(wantFields))
	}
	for f := range wantFields {
		if _, ok := props[f]; !ok {
			t.Errorf("入参字段 %q 在 schemaPlanRequest 里有，openapi 的 requestBody 没声明", f)
		}
	}
	for f := range props {
		if !wantFields[f] {
			t.Errorf("openapi 声明了入参 %q，schemaPlanRequest 里没有（真发过去会被拒成 unknown field）", f)
		}
	}
}

// serviceDDLActions 从 ../service/ddl.go 的常量定义里取全部动作。
//
// 用 AST 而不是反射：DDLAction 是字符串类型，反射只能拿到类型名拿不到常量值；
// 也不手抄一份清单——手抄的清单和 enum 一起漏，门禁就成了摆设。
func serviceDDLActions(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "service", "ddl.go"), nil, 0)
	if err != nil {
		t.Fatalf("解析 service/ddl.go: %v", err)
	}
	out := map[string]bool{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// 只看显式标了 DDLAction 的常量，别把同文件里别的字符串常量算进来。
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "DDLAction" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("动作常量 %v 不是字符串字面量", vs.Names)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("动作常量 %q: %v", lit.Value, err)
				}
				out[s] = true
			}
		}
	}
	// 认出来的太少说明解析失效了（比如动作改成集中写进 map），别让门禁静默变成摆设。
	if len(out) < 15 {
		t.Fatalf("只从 ddl.go 认出 %d 个动作，解析恐怕失效了", len(out))
	}
	return out
}

// specSchemaPlanProperties 取契约里 /schema/plan 请求体的 properties。
func specSchemaPlanProperties(t *testing.T) map[string]any {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Properties map[string]any `json:"properties"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(SpecJSON(), &doc); err != nil {
		t.Fatalf("解析契约: %v", err)
	}
	ops, ok := doc.Paths["/api/v1/connections/{id}/schema/plan"]
	if !ok {
		t.Fatalf("契约里没有 /schema/plan")
	}
	op, ok := ops["post"]
	if !ok {
		t.Fatalf("契约里 /schema/plan 没有 post")
	}
	body, ok := op.RequestBody.Content["application/json"]
	if !ok {
		t.Fatalf("契约里 /schema/plan 没有 application/json 请求体")
	}
	if len(body.Schema.Properties) == 0 {
		t.Fatalf("契约里 /schema/plan 的请求体没有 properties")
	}
	return body.Schema.Properties
}
