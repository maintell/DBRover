package api

import (
	"reflect"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/service"
)

// TestRequestStructsCoverServiceFields 把「漏字段」变成能红的用例。
//
// decode 用 DisallowUnknownFields：请求体里出现 api 结构没声明的字段会直接 400
// （json: unknown field）。而服务层测试直接调 BuildDDLPlan / BuildBulkUpdatePlan，
// 一路绿——add_check 与 create_view 就是这么在 HTTP 上不可达了两轮。
//
// 规则很简单：**服务层结构里的每个 json 字段，api 结构都必须能接**。api 可以
// 只透传不解释，但不能「接不住」。真的不需要对外暴露的字段，写进 allowlist 并
// 说明理由，而不是把规则放宽。
func TestRequestStructsCoverServiceFields(t *testing.T) {
	pairs := []struct {
		name string
		api  any
		svc  any
		// allow 是「服务层有这个字段，但刻意不从 HTTP 接」的清单。
		allow map[string]string
	}{
		{name: "schemaPlanRequest → service.DDLRequest", api: schemaPlanRequest{}, svc: service.DDLRequest{}},
		{name: "bulkPlanRequest → service.BulkUpdateRequest", api: bulkPlanRequest{}, svc: service.BulkUpdateRequest{}},
		{name: "fkPrecheckRequest → service.FKPrecheckRequest", api: fkPrecheckRequest{}, svc: service.FKPrecheckRequest{}},
		{
			name: "changePlanRequest → service.ChangeSetRequest",
			api:  changePlanRequest{}, svc: service.ChangeSetRequest{},
			allow: map[string]string{
				// 连接只能来自 URL 路径。若从请求体接 connection_id，客户端就能把
				// 一次写操作指到另一条连接上——那正是「工具参数里没有连接概念」要
				// 防的事。
				"connection_id": "连接由 URL 路径决定，不从请求体接（否则可被指向别的连接）",
			},
		},
	}

	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			apiTags := tagsOf(reflect.TypeOf(p.api))
			for tag, field := range tagsOfWithName(reflect.TypeOf(p.svc)) {
				if _, ok := apiTags[tag]; ok {
					continue
				}
				if why, allowed := p.allow[tag]; allowed {
					t.Logf("%s：服务层字段 %q（%s）刻意不从 HTTP 接：%s", p.name, tag, field, why)
					continue
				}
				t.Errorf("%s：服务层字段 %q（%s）在 api 请求结构里没有对应字段——"+
					"带这个字段的请求会以 json: unknown field 被拒。加字段，或写进 allowlist 并说明理由。",
					p.name, tag, field)
			}
		})
	}
}

// tagsOf 取结构体的 json 字段名集合（跳过 omitempty 与 -）。
func tagsOf(t reflect.Type) map[string]string {
	return tagsOfWithName(t)
}

func tagsOfWithName(t reflect.Type) map[string]string {
	out := map[string]string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // 未导出
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = f.Name
		}
		out[name] = f.Name
	}
	return out
}
