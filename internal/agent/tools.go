package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/maintell/DBRover/internal/model"
	"github.com/maintell/DBRover/internal/policy"
	"github.com/maintell/DBRover/internal/service"
)

// Tool 是一个 Agent 可调用的能力。
//
// Risk 与 Write 是两件不同的事：Risk 说这次操作有多危险（R0–R4），
// Write 说它会不会改动数据。只看 Risk 不够——只读连接上 R1 也不许写；
// 只看 Write 也不够——R0 的查询照样可能扫全表。
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Risk        policy.Level
	Write       bool
	Handle      func(ctx context.Context, args json.RawMessage) Envelope
}

// schema 返回工具的对外契约。
func (t Tool) schema() ToolSchema {
	return ToolSchema{Name: t.Name, Description: t.Description, Parameters: t.Parameters}
}

// ToolMeta 是一个工具的类别信息：名字、风险等级、是否会改数据。
//
// 刻意与 ToolSchema 分开：ToolSchema 是**给模型看的**契约，多发一个字段就是
// 多发一份提示；而「哪些工具会写」是我们自己的判据。评测集与测试靠它判断
// 「写操作有没有被执行」——任何地方都不该再手写一份写工具清单。
type ToolMeta struct {
	Name  string
	Risk  policy.Level
	Write bool
}

// ToolMetas 返回全部工具的元数据，按名字排序。
func (r *Runtime) ToolMetas() []ToolMeta {
	out := make([]ToolMeta, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, ToolMeta{Name: t.Name, Risk: t.Risk, Write: t.Write})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// schemas 返回全部工具契约，顺序稳定（模型对工具顺序敏感）。
func (r *Runtime) schemas() []ToolSchema {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ToolSchema, 0, len(names))
	for _, n := range names {
		out = append(out, r.tools[n].schema())
	}
	return out
}

// buildTools 组装 14 个工具（docs/11 §3.2）。
func (r *Runtime) buildTools() map[string]Tool {
	list := []Tool{
		r.toolConnInfo(),
		r.toolCatalogs(),
		r.toolSchemas(),
		r.toolObjects(),
		r.toolDescribe(),
		r.toolDDL(),
		r.toolSearch(),
		r.toolQueryExecute(),
		r.toolQueryExplain(),
		r.toolSample(),
		r.toolChangePlan(),
		r.toolChangeApply(),
		r.toolMonitor(),
		r.toolNote(),
	}
	out := make(map[string]Tool, len(list))
	for _, t := range list {
		out[t.Name] = t
	}
	return out
}

// ---- 参数解码 ----

// decodeArgs 严格解码参数：多余字段直接拒绝。
//
// 宽容解析的代价是模型会以为某个字段生效了，然后基于错误的前提往下走。
func decodeArgs(raw json.RawMessage, dst any) *Envelope {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		e := fail("ARGS_INVALID", err.Error(), "参数结构与工具 schema 不一致，请按 schema 重新构造")
		return &e
	}
	return nil
}

// refOf 把参数里的表名收敛成对象引用。schema 为空时由驱动按连接默认值补。
func (r *Runtime) refOf(kind model.ObjectKind, catalog, schema, name string) model.ObjectRef {
	if catalog == "" {
		catalog = r.catalog
	}
	if schema == "" {
		schema = r.schema
	}
	return model.ObjectRef{Kind: kind, Catalog: catalog, Schema: schema, Name: name}
}

// ---- 连接与元数据 ----

func (r *Runtime) toolConnInfo() Tool {
	return Tool{
		Name:        "conn.info",
		Description: "当前连接的信息：引擎、版本、只读状态、能力位、Agent 权限模式。任何任务开始时先调用它。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Risk:        policy.R0,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			if bad := decodeArgs(args, &struct{}{}); bad != nil {
				return *bad
			}
			data := map[string]any{
				"engine":       r.cat.Engine(),
				"capabilities": r.cat.Capabilities(),
				"environment":  r.env,
				"access_mode":  r.access,
				"agent_mode":   r.policy.Mode(),
				"catalog":      r.catalog,
				"schema":       r.schema,
			}
			warnings := []string{}
			if h, err := r.cat.Health(ctx); err != nil {
				warnings = append(warnings, "服务器信息读取失败："+err.Error())
			} else {
				data["version"] = h.Version
				data["server_read_only"] = h.ReadOnly
				data["timezone"] = h.Timezone
				warnings = append(warnings, h.Warnings...)
			}
			env := ok(fmt.Sprintf("%s 连接，Agent 模式 %s", r.cat.Engine(), r.policy.Mode()), data)
			env.Warnings = warnings
			return env
		},
	}
}

func (r *Runtime) toolCatalogs() Tool {
	return Tool{
		Name:        "meta.catalogs",
		Description: "列出这个连接能看到的库。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Risk:        policy.R0,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			if bad := decodeArgs(args, &struct{}{}); bad != nil {
				return *bad
			}
			list, err := r.cat.Catalogs(ctx)
			if err != nil {
				return fail("METADATA_FAILED", err.Error(), "检查连接是否还有效")
			}
			return ok(fmt.Sprintf("%d 个库", len(list)), map[string]any{"catalogs": list})
		},
	}
}

func (r *Runtime) toolSchemas() Tool {
	return Tool{
		Name:        "meta.schemas",
		Description: "列出某个库下的模式（schema）。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"catalog":{"type":"string","description":"库名，省略则用当前连接默认库"}},
			"additionalProperties":false}`),
		Risk: policy.R0,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			var in struct {
				Catalog string `json:"catalog"`
			}
			if bad := decodeArgs(args, &in); bad != nil {
				return *bad
			}
			if in.Catalog == "" {
				in.Catalog = r.catalog
			}
			list, err := r.cat.Schemas(ctx, in.Catalog)
			if err != nil {
				return fail("METADATA_FAILED", err.Error(), "确认库名是否正确")
			}
			return ok(fmt.Sprintf("%s 下 %d 个模式", in.Catalog, len(list)), map[string]any{"schemas": list})
		},
	}
}

func (r *Runtime) toolObjects() Tool {
	return Tool{
		Name:        "meta.objects",
		Description: "列出对象（表/视图）。可按名称片段过滤并分页。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"catalog":{"type":"string"},
			"schema":{"type":"string"},
			"kinds":{"type":"array","items":{"type":"string","enum":["table","view"]},"description":"省略表示两种都要"},
			"name_contains":{"type":"string","description":"按名称片段过滤，不区分大小写"},
			"limit":{"type":"integer","minimum":1,"maximum":500}},
			"additionalProperties":false}`),
		Risk: policy.R0,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			var in struct {
				Catalog      string   `json:"catalog"`
				Schema       string   `json:"schema"`
				Kinds        []string `json:"kinds"`
				NameContains string   `json:"name_contains"`
				Limit        int      `json:"limit"`
			}
			if bad := decodeArgs(args, &in); bad != nil {
				return *bad
			}
			if in.Limit <= 0 {
				in.Limit = 200
			}
			kinds := make([]model.ObjectKind, 0, len(in.Kinds))
			for _, k := range in.Kinds {
				switch k {
				case "table":
					kinds = append(kinds, model.KindTable)
				case "view":
					kinds = append(kinds, model.KindView)
				default:
					return fail("ARGS_INVALID", "不认识的种类："+k, "kinds 只能是 table 或 view")
				}
			}
			if len(kinds) == 0 {
				kinds = []model.ObjectKind{model.KindTable, model.KindView}
			}
			objs, err := r.cat.Objects(ctx, orDefault(in.Catalog, r.catalog),
				orDefault(in.Schema, r.schema), kinds...)
			if err != nil {
				return fail("METADATA_FAILED", err.Error(), "确认库名与模式名")
			}
			needle := strings.ToLower(strings.TrimSpace(in.NameContains))
			names := make([]string, 0, len(objs))
			for _, o := range objs {
				if needle != "" && !strings.Contains(strings.ToLower(o.Name), needle) {
					continue
				}
				names = append(names, o.Name)
			}
			sort.Strings(names)
			env := ok(fmt.Sprintf("%d 个对象", len(names)), map[string]any{"objects": names})
			if len(names) > in.Limit {
				env.Data = map[string]any{"objects": names[:in.Limit]}
				env.Truncated = &Truncated{
					RowsShown: in.Limit, RowsTotal: len(names),
					Hint: "用 name_contains 缩小范围",
				}
				env.Summary = fmt.Sprintf("共 %d 个对象，已返回前 %d 个", len(names), in.Limit)
			}
			env.NextActions = []string{"meta.describe", "data.sample"}
			return env
		},
	}
}

func (r *Runtime) toolDescribe() Tool {
	return Tool{
		Name:        "meta.describe",
		Description: "一张表或视图的完整结构：列、类型、可空、默认值、主键、索引、外键、检查约束。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"对象名"},
			"catalog":{"type":"string"},
			"schema":{"type":"string"},
			"kind":{"type":"string","enum":["table","view"]}},
			"required":["name"],"additionalProperties":false}`),
		Risk: policy.R0,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			var in struct {
				Name    string `json:"name"`
				Catalog string `json:"catalog"`
				Schema  string `json:"schema"`
				Kind    string `json:"kind"`
			}
			if bad := decodeArgs(args, &in); bad != nil {
				return *bad
			}
			if strings.TrimSpace(in.Name) == "" {
				return fail("ARGS_INVALID", "name 不能为空", "先 meta.objects 找到对象名")
			}
			kind := model.KindTable
			if in.Kind == "view" {
				kind = model.KindView
			}
			td, err := r.cat.DescribeTable(ctx, r.refOf(kind, in.Catalog, in.Schema, in.Name))
			if err != nil {
				return fail("OBJECT_NOT_FOUND", err.Error(), "确认对象名；用 meta.objects 列出可用对象")
			}
			return ok(fmt.Sprintf("%s：%d 列，主键 %s", td.Ref.Name, len(td.Columns),
				strings.Join(td.PrimaryKeyColumns(), ",")), td)
		},
	}
}

func (r *Runtime) toolDDL() Tool {
	return Tool{
		Name:        "meta.ddl",
		Description: "返回对象的结构原文（CREATE 语句）。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string"},
			"catalog":{"type":"string"},
			"schema":{"type":"string"},
			"kind":{"type":"string","enum":["table","view"]}},
			"required":["name"],"additionalProperties":false}`),
		Risk: policy.R0,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			var in struct {
				Name    string `json:"name"`
				Catalog string `json:"catalog"`
				Schema  string `json:"schema"`
				Kind    string `json:"kind"`
			}
			if bad := decodeArgs(args, &in); bad != nil {
				return *bad
			}
			kind := model.KindTable
			if in.Kind == "view" {
				kind = model.KindView
			}
			ddl, err := r.exec.TableDDL(ctx, r.refOf(kind, in.Catalog, in.Schema, in.Name))
			if err != nil {
				return fail("OBJECT_NOT_FOUND", err.Error(), "确认对象名与种类")
			}
			return ok(in.Name+" 的结构原文", map[string]any{"ddl": ddl})
		},
	}
}

func (r *Runtime) toolSearch() Tool {
	return Tool{
		Name:        "meta.search",
		Description: "按名称或注释模糊搜索对象，跨库。找表时用业务词（如「未支付」「对账」）往往比猜表名有效。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"pattern":{"type":"string"},
			"limit":{"type":"integer","minimum":1,"maximum":200}},
			"required":["pattern"],"additionalProperties":false}`),
		Risk: policy.R0,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			var in struct {
				Pattern string `json:"pattern"`
				Limit   int    `json:"limit"`
			}
			if bad := decodeArgs(args, &in); bad != nil {
				return *bad
			}
			if in.Limit <= 0 {
				in.Limit = 50
			}
			objs, err := r.cat.Search(ctx, in.Pattern, in.Limit)
			if err != nil {
				return fail("METADATA_FAILED", err.Error(), "换一个关键词试试")
			}
			names := make([]string, 0, len(objs))
			for _, o := range objs {
				names = append(names, string(o.Kind)+":"+o.Name)
			}
			return ok(fmt.Sprintf("命中 %d 个", len(names)), map[string]any{"objects": names})
		},
	}
}

// ---- 查询 ----

func (r *Runtime) toolQueryExecute() Tool {
	return Tool{
		Name: "query.execute",
		Description: "执行一条**只读** SQL（SELECT/SHOW/EXPLAIN 等），返回摘要与前若干行。" +
			"写语句会被拒绝，请改用 change.plan。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"sql":{"type":"string"},
			"max_rows":{"type":"integer","minimum":1,"maximum":5000}},
			"required":["sql"],"additionalProperties":false}`),
		Risk: policy.R0,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			var in struct {
				SQL     string `json:"sql"`
				MaxRows int    `json:"max_rows"`
			}
			if bad := decodeArgs(args, &in); bad != nil {
				return *bad
			}
			if in.MaxRows <= 0 {
				in.MaxRows = 200
			}
			out, err := r.exec.ReadOnlyQuery(ctx, service.Request{
				SQL: in.SQL, MaxRows: in.MaxRows, Actor: r.actor,
			})
			if err != nil {
				if service.IsReadOnlyViolation(err) {
					return fail("READ_ONLY_REQUIRED", err.Error(),
						"写操作要先 change.plan 生成预案、再经人工审批后 change.apply")
				}
				return fail("SQL_FAILED", err.Error(), "检查语法与对象名")
			}
			if out.Err != nil {
				return fail(out.Err.Code, out.Err.Message, out.Err.Hint)
			}
			if out.Blocked {
				return blockEnvelope(out)
			}
			if len(out.Results) == 0 {
				return fail("EMPTY_RESULT", "语句没有返回结果", "确认语句是否为查询")
			}
			// 行数正好等于上限说明可能还有更多：这时报「共 N 行」是撒谎。
			capped := len(out.Results[0].Rows) >= in.MaxRows
			return summarizeResult(out.Results[0], capped)
		},
	}
}

func (r *Runtime) toolQueryExplain() Tool {
	return Tool{
		Name:        "query.explain",
		Description: "取一条查询的执行计划（不真正执行）。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"sql":{"type":"string"}},"required":["sql"],"additionalProperties":false}`),
		Risk: policy.R0,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			var in struct {
				SQL string `json:"sql"`
			}
			if bad := decodeArgs(args, &in); bad != nil {
				return *bad
			}
			out, err := r.exec.Explain(ctx, service.Request{SQL: in.SQL, Actor: r.actor})
			if err != nil {
				if err == service.ErrExplainUnsupported {
					return fail("EXPLAIN_UNSUPPORTED", err.Error(),
						"这个引擎要用别的开关（如 SET SHOWPLAN_ALL）看计划，请让用户人工查看")
				}
				return fail("SQL_FAILED", err.Error(), "检查语句本身能否执行")
			}
			if out.Err != nil {
				return fail(out.Err.Code, out.Err.Message, out.Err.Hint)
			}
			if out.Blocked {
				return blockEnvelope(out)
			}
			if len(out.Results) == 0 {
				return fail("EMPTY_RESULT", "执行计划为空", "确认语句本身可执行")
			}
			return summarizeResult(out.Results[0], false)
		},
	}
}

func (r *Runtime) toolSample() Tool {
	return Tool{
		Name:        "data.sample",
		Description: "按条件取一张表的样例行（走键集分页，不需要写 LIMIT）。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"table":{"type":"string"},
			"schema":{"type":"string"},
			"limit":{"type":"integer","minimum":1,"maximum":200},
			"filter":{"type":"array","items":{"type":"object","properties":{
				"column":{"type":"string"},
				"op":{"type":"string","enum":["eq","ne","gt","ge","lt","le","like","in","between","is_null","is_not_null"]},
				"value":{},
				"values":{"type":"array","items":{"type":"string"}}},
				"required":["column","op"],"additionalProperties":false}}},
			"required":["table"],"additionalProperties":false}`),
		Risk: policy.R0,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			var in struct {
				Table  string               `json:"table"`
				Schema string               `json:"schema"`
				Limit  int                  `json:"limit"`
				Filter []service.FilterCond `json:"filter"`
			}
			if bad := decodeArgs(args, &in); bad != nil {
				return *bad
			}
			if in.Limit <= 0 {
				in.Limit = 20
			}
			res, err := r.exec.TableRows(ctx, service.TableRowsRequest{
				Table: in.Table, Limit: in.Limit, Filter: in.Filter,
			})
			if err != nil {
				return fail("SAMPLE_FAILED", err.Error(),
					"表必须存在且有主键（键集分页依赖主键）；筛选项只能用表里真实存在的列")
			}
			names := make([]string, len(res.Columns))
			for i, c := range res.Columns {
				names[i] = c.Name
			}
			env := ok(fmt.Sprintf("%s：%d 行", in.Table, len(res.Rows)), map[string]any{
				"columns": names, "rows": clipRows(res.Rows, 50),
			})
			if res.HasMore {
				env.Truncated = &Truncated{RowsShown: len(res.Rows), RowsTotal: -1,
					Hint: "还有更多行；加 filter 缩小范围，或用 query.execute 做聚合"}
			}
			return env
		},
	}
}

// ---- 变更 ----

func (r *Runtime) toolChangePlan() Tool {
	return Tool{
		Name: "change.plan",
		Description: "生成变更预案（不执行）。changes 走行级变更，ddl 走结构变更，二选一。" +
			"只读模式下也能用：预案只由服务端保管，可以带走给人看。" +
			"执行必须另走 change.apply，且风险 R2 以上要经人工批准。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"table":{"type":"object","description":"目标表引用（ddl 自带 table 时可省略）","properties":{
				"kind":{"type":"string"},"catalog":{"type":"string"},
				"schema":{"type":"string"},"name":{"type":"string"}},
				"additionalProperties":false},
			"changes":{"type":"array","description":"行级变更","items":{"type":"object","properties":{
				"op":{"type":"string","enum":["update","delete","insert"]},
				"key":{"type":"object"},
				"before":{"type":"object"},
				"after":{"type":"object"}},
				"required":["op"],"additionalProperties":false}},
			"ddl":{"type":"object","description":"结构变更请求（与 changes 二选一），形状同 /schema/plan 的请求体",
				"properties":{
					"table":{"type":"object","properties":{
						"kind":{"type":"string"},"catalog":{"type":"string"},
						"schema":{"type":"string"},"name":{"type":"string"}},"additionalProperties":false},
					"action":{"type":"string","enum":["create_table","add_column","drop_column",
						"alter_column_type","create_index","drop_index","add_foreign_key","drop_table","raw"]},
					"column":{"type":"object"},"columns":{"type":"array"},"primary_key":{"type":"array"},
					"index":{"type":"object"},"foreign_key":{"type":"object"},"sql":{"type":"string"}},
				"additionalProperties":false}},"additionalProperties":false}`),
		// 生成预案本身不写数据，所以算 R0 且不是写工具：只读模式下必须能用
		// （docs/11 §5.1 硬约束 2——预案可以生成给人带走，只是不能执行）。
		Risk:  policy.R0,
		Write: false,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			var in struct {
				Table   model.ObjectRef     `json:"table"`
				Changes []service.RowChange `json:"changes"`
				DDL     json.RawMessage     `json:"ddl"`
			}
			if bad := decodeArgs(args, &in); bad != nil {
				return *bad
			}
			hasDDL := len(in.DDL) > 0 && string(in.DDL) != "null"
			if hasDDL && len(in.Changes) > 0 {
				return fail("ARGS_INVALID", "changes 与 ddl 只能给一个",
					"行级变更用 changes，结构变更用 ddl")
			}

			var plan *service.ChangePlan
			var err error
			if hasDDL {
				var req service.DDLRequest
				if bad := decodeArgs(in.DDL, &req); bad != nil {
					return *bad
				}
				if req.Table.Name == "" {
					req.Table = in.Table
				}
				req.Table.Catalog = orDefault(req.Table.Catalog, r.catalog)
				req.Table.Schema = orDefault(req.Table.Schema, r.schema)
				if req.Table.Kind == "" {
					req.Table.Kind = model.KindTable
				}
				plan, err = r.exec.BuildDDLPlan(ctx, req)
				if err != nil {
					return fail("PLAN_FAILED", err.Error(),
						"结构变更前会先跑依赖与收窄预检；被拦下时先解决原因再生成预案")
				}
			} else {
				if len(in.Changes) == 0 {
					return fail("ARGS_INVALID", "changes 与 ddl 都没有给",
						"行级变更给 changes；建表/加列/改索引给 ddl")
				}
				cs := service.ChangeSetRequest{Table: in.Table, Changes: in.Changes}
				cs.Table.Catalog = orDefault(cs.Table.Catalog, r.catalog)
				cs.Table.Schema = orDefault(cs.Table.Schema, r.schema)
				if cs.Table.Kind == "" {
					cs.Table.Kind = model.KindTable
				}
				plan, err = r.exec.BuildChangePlan(ctx, cs)
				if err != nil {
					return fail("PLAN_FAILED", err.Error(),
						"变更集要求目标表有主键；key/before/after 里的值要与列类型匹配")
				}
			}

			r.rememberPlan(plan)
			env := ok(fmt.Sprintf("预案 %s：%s，影响 %s 行，%s", plan.ID, plan.Risk,
				rowsText(plan.AffectedRows), plan.Friction), map[string]any{
				"plan_id":       plan.ID,
				"risk":          plan.Risk,
				"friction":      plan.Friction,
				"ddl":           plan.DDL,
				"affected_rows": plan.AffectedRows,
				"irreversible":  plan.Irreversible,
				"statements":    plan.Statements,
				"rollback":      plan.Rollback,
				"notes":         plan.Notes,
			})
			env.NextActions = []string{"change.apply"}
			if plan.Risk >= policy.R2 {
				env.Warnings = append(env.Warnings,
					"这张预案是 "+plan.Risk.String()+"，执行前必须由用户批准")
			} else {
				env.Warnings = append(env.Warnings, "预案只由服务端保管；执行会改数据")
			}
			return env
		},
	}
}
func (r *Runtime) toolChangeApply() Tool {
	return Tool{
		Name: "change.apply",
		Description: "执行一张已存在的预案。预案必须是本轮 change.plan 生成的，且已由用户批准；" +
			"Agent 自己不能批准。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"plan_id":{"type":"string"}},
			"required":["plan_id"],"additionalProperties":false}`),
		Risk:  policy.R2,
		Write: true,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			var in struct {
				PlanID string `json:"plan_id"`
			}
			if bad := decodeArgs(args, &in); bad != nil {
				return *bad
			}
			plan, okPlan := r.lookupPlan(in.PlanID)
			if !okPlan {
				return fail("PLAN_NOT_FOUND", "没有这张预案：", "先用 change.plan 生成预案")
			}
			// 审批门槛按预案自己的风险算（docs/11 §5.2）：R1 及以下在非自主模式下
			// 也直接放行，否则每改一行都要人点一次，人会开始盲点。
			needsApproval := plan.Risk >= policy.R2 && r.policy.Mode() != ModeAutonomous
			if needsApproval && !r.isApproved(plan.ID) {
				return fail("APPROVAL_REQUIRED",
					"预案 "+plan.ID+" 是 "+plan.Risk.String()+"，还没有经过人工确认",
					"请把最终 SQL 与回滚语句展示给用户并等待确认；Agent 无法自行批准")
			}
			if time.Now().After(plan.ExpiresAt) {
				return fail("PLAN_EXPIRED", "预案已过期", "重新生成预案")
			}
			out, err := r.exec.ApplyChangePlan(ctx, plan, service.Request{
				Approval: true, Actor: r.actor,
			})
			if err != nil {
				return fail("APPLY_FAILED", err.Error(), "确认结构没有变化；必要时重新读一次表结构")
			}
			if out.Err != nil {
				return fail(out.Err.Code, out.Err.Message, out.Err.Hint)
			}
			r.forgetPlan(plan.ID)
			return ok(fmt.Sprintf("已执行：影响 %d 行", out.RowsAffected), map[string]any{
				"execution_id":  out.ExecutionID,
				"rows_affected": out.RowsAffected,
				"risk":          out.Risk,
				"rollback":      plan.Rollback,
			})
		},
	}
}

// ---- 观测与笔记 ----

func (r *Runtime) toolMonitor() Tool {
	return Tool{
		Name:        "monitor.snapshot",
		Description: "当前库的会话快照：等锁的会话、开了很久的事务。",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Risk:        policy.R0,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			if bad := decodeArgs(args, &struct{}{}); bad != nil {
				return *bad
			}
			list, err := r.cat.Blockers(ctx, time.Minute)
			if err != nil {
				return fail("MONITOR_FAILED", err.Error(), "这个引擎可能不支持会话视图")
			}
			env := ok(fmt.Sprintf("%d 个值得注意的会话", len(list)),
				map[string]any{"sessions": list})
			if len(list) == 0 {
				env.Summary = "没有发现等锁或超长事务"
			}
			return env
		},
	}
}

func (r *Runtime) toolNote() Tool {
	return Tool{
		Name:        "note.write",
		Description: "把一条结论写进本次会话的笔记（给用户看的重点，不要写过程流水账）。",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`),
		Risk: policy.R0,
		Handle: func(ctx context.Context, args json.RawMessage) Envelope {
			var in struct {
				Text string `json:"text"`
			}
			if bad := decodeArgs(args, &in); bad != nil {
				return *bad
			}
			if strings.TrimSpace(in.Text) == "" {
				return fail("ARGS_INVALID", "text 不能为空", "写一句能直接给用户看的结论")
			}
			r.mu.Lock()
			r.notes = append(r.notes, in.Text)
			r.mu.Unlock()
			return ok("已记下", map[string]any{"notes": len(r.notes)})
		},
	}
}

// blockEnvelope 把「执行门拦下了」翻成工具信封。
//
// 不提拦截原因的话，模型只会看到「空结果」，然后按「语句是不是查询」去猜——
// 猜错方向就白烧一轮。具体预检给了码就用它（如跨库引用），否则归到策略类。
func blockEnvelope(out *service.Outcome) Envelope {
	if out.BlockCode != "" {
		return fail(out.BlockCode, out.Reason, out.BlockHint)
	}
	return fail("POLICY_BLOCKED", out.Reason,
		"换一个可执行的写法；如果确实是写操作，请走 change.plan 与人工审批")
}

// ---- 结果摘要 ----

// summarizeResult 把一个语句结果压成信封，按 docs/11 §3.4 分档截断。
func summarizeResult(res service.StatementResult, capped bool) Envelope {
	names := make([]string, len(res.Columns))
	for i, c := range res.Columns {
		names[i] = c.Name
	}
	total := len(res.Rows)
	limit := 50
	if total > 1000 {
		limit = 20
	}
	data := map[string]any{
		"columns": names,
		"rows":    clipRows(res.Rows, limit),
	}
	env := ok(fmt.Sprintf("%d 行 × %d 列，%d ms", total, len(names), res.DurationMS), data)
	switch {
	case capped:
		env.Truncated = &Truncated{RowsShown: limit, RowsTotal: -1,
			Hint: "结果被行数上限截断，可能还有更多：加筛选条件或改用聚合查询"}
		env.Warnings = append(env.Warnings, "已取到本次上限，行数不是全量")
	case total > 1000:
		env.Truncated = &Truncated{RowsShown: limit, RowsTotal: total,
			Hint: "结果过大：改用 GROUP BY / COUNT 做聚合，或加筛选条件"}
		env.Warnings = append(env.Warnings, "只返回了前 "+itoa(limit)+" 行")
	case total > limit:
		env.Truncated = &Truncated{RowsShown: limit, RowsTotal: total,
			Hint: "还有更多行；需要结论请用聚合查询，需要明细请加筛选条件"}
	}
	if stats := columnStats(res.Columns, res.Rows); stats != nil {
		data["stats"] = stats
	}
	return env
}

// clipRows 截断结果集。
func clipRows(rows [][]any, limit int) [][]any {
	if len(rows) <= limit {
		return rows
	}
	return rows[:limit]
}

// columnStats 给出每列的粗略画像。
//
// 目的不是统计严谨，而是让模型在只看到前 50 行时也知道后面大概长什么样：
// 数值列给 min/max，其他列给去重后的取值个数与前 3 个高频值。
func columnStats(cols []model.Column, rows [][]any) map[string]any {
	if len(rows) <= 50 {
		return nil
	}
	out := make(map[string]any, len(cols))
	for i, c := range cols {
		var min, max *float64
		distinct := map[string]int{}
		for _, r := range rows {
			if i >= len(r) {
				continue
			}
			switch v := r[i].(type) {
			case float64:
				if min == nil || v < *min {
					x := v
					min = &x
				}
				if max == nil || v > *max {
					x := v
					max = &x
				}
			case nil:
				distinct["<null>"]++
			default:
				distinct[fmt.Sprint(v)]++
			}
		}
		info := map[string]any{"distinct": len(distinct)}
		if min != nil && max != nil {
			info["min"] = *min
			info["max"] = *max
		}
		if len(distinct) > 0 {
			info["top"] = topValues(distinct, 3)
		}
		out[c.Name] = info
	}
	return out
}

func topValues(counts map[string]int, n int) []string {
	type kv struct {
		k string
		v int
	}
	all := make([]kv, 0, len(counts))
	for k, v := range counts {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	if len(all) > n {
		all = all[:n]
	}
	out := make([]string, len(all))
	for i, x := range all {
		out[i] = x.k
	}
	return out
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func itoa(n int) string { return fmt.Sprint(n) }

func rowsText(n int64) string {
	if n < 0 {
		return "未知"
	}
	return itoa(int(n))
}
