// Package policy 是权限与风险的唯一判定点。
//
// 两个正交维度：
//   - Level：语句本身的危险性（R0–R4），可离线判定；
//   - Friction：执行前打扰强度（F0–F5），由「环境 × 风险」合成。
package policy

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/maintell/DBRover/internal/driver/dialect"
)

// Level 是语句风险分级。
type Level int

const (
	R0 Level = iota // 只读
	R1              // 单行/小批量写，命中主键
	R2              // 有条件但未确定命中唯一索引的写
	R3              // DDL
	R4              // 不可逆或全表
)

func (l Level) String() string { return fmt.Sprintf("R%d", int(l)) }

// MarshalJSON 让等级在 JSON 中输出为 "R0" 而不是 0，
// 避免调用方按数字猜测含义。
func (l Level) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }

// UnmarshalJSON 让 "R1" 能读回 Level（与 MarshalJSON 对称，审计链验证要用）。
func (l *Level) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(s), "R"))
	if err != nil {
		return fmt.Errorf("policy: 无法解析风险等级 %q", s)
	}
	*l = Level(n)
	return nil
}

// Grade 是分级结果。
//
// 带 json tag：它会随 /exec 的每条语句结果出去，而契约里所有字段都是
// snake_case。没有 tag 时编码成 {"Level":…,"Reason":…,"Writes":…}，
// 客户端按文档写的 grade.level 会取到 undefined——静默失效，最难查。
type Grade struct {
	Level  Level  `json:"level"`
	Reason string `json:"reason"`
	Writes bool   `json:"writes"` // 该语句是否会修改数据或结构
}

// Relations 提供分级所需的对象元数据。
// 未知时返回 nil —— 分级会保守降级为 R2 而不是 R1。
type Relations interface {
	PrimaryKey(table string) []string
}

// StaticRelations 是测试与无元数据场景的实现。
type StaticRelations map[string][]string

// PrimaryKey 返回表的主键列（小写）。键大小写不敏感。
func (s StaticRelations) PrimaryKey(table string) []string {
	if s == nil || table == "" {
		return nil
	}
	return s[strings.ToLower(table)]
}

// GradeStatement 判定单条语句的风险等级。
//
// 解析失败一律判定 R4（保守）：读不动的语句不允许被当作只读放行。
func GradeStatement(d dialect.Dialect, sqlText string, rel Relations) Grade {
	a, err := d.Analyze(sqlText)
	if err != nil {
		return Grade{Level: R4, Reason: "无法解析语句，保守判定为 R4", Writes: true}
	}

	switch a.Class {
	case dialect.ClassUnknown:
		return Grade{Level: R4, Reason: "无法识别的语句类型，保守判定为 R4", Writes: true}
	case dialect.ClassTCL:
		return Grade{Level: R0, Reason: "事务控制语句", Writes: false}
	case dialect.ClassDCL:
		return Grade{Level: R3, Reason: "权限变更", Writes: true}
	case dialect.ClassQuery:
		return gradeQuery(a)
	case dialect.ClassDML:
		return gradeDML(a, rel)
	case dialect.ClassDDL:
		return gradeDDL(a)
	}
	return Grade{Level: R4, Reason: "无法判定，保守判定为 R4", Writes: true}
}

func gradeQuery(a dialect.Analysis) Grade {
	switch {
	case a.WriteInCTE:
		return Grade{Level: R2, Reason: "CTE 内包含写语句", Writes: true}
	case a.SelectInto:
		return Grade{Level: R3, Reason: "SELECT ... INTO 会写入表", Writes: true}
	case a.CopyFrom:
		return Grade{Level: R3, Reason: "COPY ... FROM 会写入数据", Writes: true}
	case a.ForUpdate:
		return Grade{Level: R2, Reason: "SELECT ... FOR UPDATE 会加行锁", Writes: false}
	}
	return Grade{Level: R0, Reason: "只读查询", Writes: false}
}

func gradeDML(a dialect.Analysis, rel Relations) Grade {
	switch a.Verb {
	case "TRUNCATE":
		return Grade{Level: R4, Reason: "TRUNCATE 清空表，多数引擎不可回滚", Writes: true}
	case "UPDATE", "DELETE":
		if !a.HasWhere {
			return Grade{Level: R4, Reason: a.Verb + " 无 WHERE 条件，将影响全表", Writes: true}
		}
		if a.Condition.AlwaysTrue {
			return Grade{Level: R4, Reason: a.Verb + " 的 WHERE 为恒真条件，等价于全表", Writes: true}
		}
		// 顶层 OR/NOT 会让「有等值」失去意义：id=1 OR 1=1 也是等值。
		if a.Condition.HasDisjunction {
			return Grade{Level: R2, Reason: a.Verb + " 的 WHERE 含 OR/NOT，无法确认命中唯一索引", Writes: true}
		}
		// 只有单列等值（且无析取）才算命中主键。IN 列表可以任意长，不降低等级。
		if pk := pkOf(rel, a.TargetTable); len(pk) > 0 && hitsEquals(a.Condition, pk) {
			return Grade{Level: R1, Reason: a.Verb + " 命中主键等值条件", Writes: true}
		}
		return Grade{Level: R2, Reason: a.Verb + " 有条件，但未确定命中唯一索引", Writes: true}
	case "INSERT", "REPLACE", "UPSERT":
		return Grade{Level: R1, Reason: "插入单行", Writes: true}
	case "MERGE":
		return Grade{Level: R3, Reason: "MERGE 可能同时更新与删除", Writes: true}
	case "COPY":
		return Grade{Level: R3, Reason: "COPY ... FROM 会写入数据", Writes: true}
	}
	return Grade{Level: R2, Reason: "未细分的写语句", Writes: true}
}

func gradeDDL(a dialect.Analysis) Grade {
	if a.Verb == "TRUNCATE" {
		return Grade{Level: R4, Reason: "TRUNCATE 不可回滚", Writes: true}
	}
	if a.Verb == "DROP" {
		switch a.Object {
		case "TABLE", "VIEW", "SCHEMA", "DATABASE", "TYPE", "DOMAIN", "SEQUENCE", "FUNCTION", "PROCEDURE":
			return Grade{Level: R4, Reason: "DROP " + a.Object + " 不可逆", Writes: true}
		case "INDEX", "TRIGGER":
			return Grade{Level: R3, Reason: "DROP " + a.Object + " 可通过重建恢复", Writes: true}
		}
		return Grade{Level: R4, Reason: "DROP 未识别对象，保守判定 R4", Writes: true}
	}
	return Grade{Level: R3, Reason: "结构变更", Writes: true}
}

func pkOf(rel Relations, table string) []string {
	if rel == nil || table == "" {
		return nil
	}
	return rel.PrimaryKey(table)
}

func hitsEquals(c dialect.Condition, pk []string) bool {
	for _, col := range pk {
		lc := strings.ToLower(col)
		for _, e := range c.Equals {
			if e == lc {
				return true
			}
		}
	}
	return false
}
