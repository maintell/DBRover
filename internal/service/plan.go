package service

import (
	"fmt"
	"strings"

	"github.com/maintell/DBRover/internal/model"
)

// PlanFinding 是执行计划里「值得让人看一眼」的一个点。
//
// 它不是错误，只是「这条语句的代价可能不是你以为的那样」。
// 判据只做**引擎自己说过的话**的翻译：把 SCAN / Seq Scan / type=ALL
// 翻成人话，而不是自己估算代价——自己算的代价在跨引擎时必然是错的。
type PlanFinding struct {
	// Severity 取 info / warn；warn 表示这一条可能会在大表上失控。
	Severity string `json:"severity"`
	// Code 稳定可判（FULL_SCAN / NO_INDEX / FILESORT / TEMP_TABLE / BLOCK_NESTED_LOOP）。
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// AnalyzePlan 从执行计划的行里挑出偏差节点。
//
// 三种引擎的计划形状完全不同：SQLite 是一张 id/parent/detail 表，
// MySQL 是 id/select_type/table/type/key/rows/Extra 表，PostgreSQL 是纯文本行。
// 所以这里按引擎分支，只读「引擎自己给的字段」，不猜。
//
// 同一张表被扫多次是常见的（自连接、子查询），findings 按 code+message 去重。
func AnalyzePlan(engine model.Engine, cols []model.Column, rows [][]any) []PlanFinding {
	if len(rows) == 0 {
		return nil
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = strings.ToLower(strings.TrimSpace(c.Name))
	}
	// cellOf 按列名取一格；列不存在时返回空串（不同版本的列可能不同）。
	cellOf := func(row []any, name string) string {
		for i, n := range names {
			if n == name && i < len(row) {
				return cellText(row[i])
			}
		}
		return ""
	}
	lastCell := func(row []any) string {
		if len(row) == 0 {
			return ""
		}
		return cellText(row[len(row)-1])
	}

	var out []PlanFinding
	switch engine {
	case model.EngineSQLite:
		for _, row := range rows {
			detail := lastCell(row)
			up := strings.ToUpper(detail)
			switch {
			case strings.HasPrefix(up, "SCAN "):
				out = append(out, PlanFinding{
					Severity: "warn", Code: "FULL_SCAN",
					Message: "全表扫描：" + detail,
					Hint:    "给 WHERE / JOIN 用到的列建索引，让计划变成 SEARCH … USING INDEX",
				})
			case strings.Contains(up, "USE TEMP B-TREE"):
				out = append(out, PlanFinding{
					Severity: "info", Code: "TEMP_TABLE",
					Message: "用到临时 B 树：" + detail,
					Hint:    "排序或去重无法走索引；数据量大时会落盘",
				})
			}
		}

	case model.EnginePostgres:
		for _, row := range rows {
			line := lastCell(row)
			switch {
			case strings.Contains(line, "Seq Scan on "):
				out = append(out, PlanFinding{
					Severity: "warn", Code: "FULL_SCAN",
					Message: "全表扫描：" + strings.TrimSpace(line),
					Hint:    "检查 WHERE 条件是否能命中索引；刚导入大量数据后可先 ANALYZE",
				})
			case strings.Contains(line, "Nested Loop"):
				out = append(out, PlanFinding{
					Severity: "info", Code: "BLOCK_NESTED_LOOP",
					Message: "嵌套循环连接：" + strings.TrimSpace(line),
					Hint:    "内层若是全表扫描，行数一多代价会按乘积增长",
				})
			}
		}

	case model.EngineMySQL, model.EngineMariaDB:
		for _, row := range rows {
			typ := strings.ToLower(cellOf(row, "type"))
			extra := strings.ToLower(cellOf(row, "extra"))
			table := cellOf(row, "table")
			switch {
			case typ == "all":
				out = append(out, PlanFinding{
					Severity: "warn", Code: "FULL_SCAN",
					Message: fmt.Sprintf("全表扫描：%s（type=ALL）", table),
					Hint:    "给 WHERE / JOIN 用到的列建索引，或把条件改成能命中的形式",
				})
			case typ == "index" && cellOf(row, "key") != "":
				out = append(out, PlanFinding{
					Severity: "info", Code: "INDEX_SCAN",
					Message: fmt.Sprintf("扫整个索引：%s（type=index，key=%s）", table, cellOf(row, "key")),
					Hint:    "比全表扫描好，但仍读了整棵索引；加更精确的 WHERE 可以收窄",
				})
			}
			if strings.Contains(extra, "using filesort") {
				out = append(out, PlanFinding{
					Severity: "info", Code: "FILESORT",
					Message: fmt.Sprintf("额外排序：%s（Using filesort）", table),
					Hint:    "ORDER BY 没走索引；数据量大时会落盘排序",
				})
			}
			if strings.Contains(extra, "using temporary") {
				out = append(out, PlanFinding{
					Severity: "info", Code: "TEMP_TABLE",
					Message: fmt.Sprintf("临时表：%s（Using temporary）", table),
					Hint:    "GROUP BY / DISTINCT / UNION 建了临时表；考虑改写或加索引",
				})
			}
		}
	}

	return dedupeFindings(out, 8)
}

// cellText 把一格值转成文本。计划里的字段类型各家不同（字符串、[]byte、数字）。
func cellText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

// dedupeFindings 按 code+message 去重并截断。
//
// 截断的理由是界面：一个 400 行的计划能刷出几十条同样的「全表扫描」，
// 那不是信息，是噪音。
func dedupeFindings(in []PlanFinding, limit int) []PlanFinding {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]PlanFinding, 0, len(in))
	for _, f := range in {
		k := f.Code + "|" + f.Message
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
		if len(out) >= limit {
			break
		}
	}
	return out
}
