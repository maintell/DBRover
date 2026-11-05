package api_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/maintell/DBRover/internal/model"
)

// TestSchemaResponseCoversTableDetail 守住「结构页拿到的字段与 TableDetail 一样多」。
//
// 这个响应是自建 DTO，TableDetail 新增字段不会自动带过去——漏一次就是「后端有、
// 界面没有」，而且编译与用例都不会报错。checks 就这么漏过：结构页能加约束，
// 却从来没列过已有的约束。判据取自动作定义处（结构体的 json 标签），不从响应自证。
func TestSchemaResponseCoversTableDetail(t *testing.T) {
	_, ts, dir := newServer(t)
	connID := createConn(t, ts, dir, "test", "read-write")
	execSQL(t, ts, connID, "CREATE TABLE t (id INTEGER PRIMARY KEY)")

	code, body := do(t, ts, http.MethodGet, "/api/v1/connections/"+connID+"/tables/t/schema", nil, true)
	if code != http.StatusOK {
		t.Fatalf("schema = %d %v", code, body)
	}

	// ref 在响应里叫 table（结构页一直用这个名字），其余字段名与 json 标签一致。
	alias := map[string]string{"ref": "table"}
	for _, name := range tableDetailJSONKeys(t) {
		key := name
		if a, ok := alias[name]; ok {
			key = a
		}
		if _, ok := body[key]; !ok {
			t.Errorf("schema 响应缺字段 %q（TableDetail 有、响应没带）", key)
		}
	}
}

// tableDetailJSONKeys 从 model.TableDetail 的 json 标签取字段名。
func tableDetailJSONKeys(t *testing.T) []string {
	t.Helper()
	typ := reflect.TypeOf(model.TableDetail{})
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		out = append(out, strings.Split(tag, ",")[0])
	}
	if len(out) < 8 {
		t.Fatalf("只从 TableDetail 里认出 %d 个字段，解析恐怕失效了", len(out))
	}
	return out
}
