package dialect

import "strings"

// keywords 是归一化时统一为大写的关键字集合。
// 未列入的标识符保持原样 —— 表名/列名的大小写在部分引擎里是有意义的。
var keywords = map[string]bool{}

func init() {
	words := []string{
		"ADD", "ALL", "ALTER", "AND", "ANY", "AS", "ASC", "BEGIN", "BETWEEN", "BY",
		"CASCADE", "CASE", "CAST", "CHECK", "COLLATE", "COLUMN", "COMMIT", "CONSTRAINT",
		"CREATE", "CROSS", "CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP",
		"DATABASE", "DEFAULT", "DELETE", "DESC", "DISTINCT", "DROP", "ELSE", "END",
		"EXCEPT", "EXISTS", "EXPLAIN", "FALSE", "FETCH", "FIRST", "FOR", "FOREIGN",
		"FROM", "FULL", "FUNCTION", "GRANT", "GROUP", "HAVING", "IF", "IN", "INDEX",
		"INNER", "INSERT", "INTERSECT", "INTO", "IS", "JOIN", "KEY", "LEFT", "LIKE",
		"LIMIT", "LOCK", "MATERIALIZED", "MERGE", "NEXT", "NOT", "NULL", "OFFSET",
		"ON", "ONLY", "OR", "ORDER", "OUTER", "PRIMARY", "PROCEDURE", "RECURSIVE",
		"REFERENCES", "RENAME", "REPLACE", "RETURNING", "REVOKE", "RIGHT", "ROLLBACK",
		"ROW", "ROWID", "ROWNUM", "ROWS", "SAVEPOINT", "SCHEMA", "SELECT", "SET",
		"SHOW", "TABLE", "THEN", "TO", "TOP", "TRANSACTION", "TRIGGER", "TRUE",
		"TRUNCATE", "UNION", "UNIQUE", "UNLOGGED", "UPDATE", "USING", "VACUUM",
		"VALUES", "VIEW", "WHEN", "WHERE", "WINDOW", "WITH", "ANALYZE", "VERBOSE",
		"DESCRIBE", "PRAGMA", "COPY", "STDIN", "STDOUT", "DELIMITER",
	}
	for _, w := range words {
		keywords[w] = true
	}
}

// IsKeyword 报告标识符是否为 SQL 关键字。
func IsKeyword(s string) bool { return keywords[strings.ToUpper(s)] }

// Normalize 生成 SQL 指纹用文本：字面量与参数替换为 ?，
// 关键字大写，标识符保持原样，IN 列表折叠。
func (d impl) Normalize(sqlText string) (string, error) {
	toks, err := Tokenize(sqlText, d.Syntax())
	if err != nil {
		return "", err
	}
	toks = FilterComments(toks)
	if len(toks) == 0 {
		return "", nil
	}

	vals := make([]string, 0, len(toks))
	kinds := make([]TokKind, 0, len(toks))
	for _, t := range toks {
		switch t.Kind {
		case TokString, TokNumber, TokParam:
			vals = append(vals, "?")
			kinds = append(kinds, TokParam)
		case TokIdent:
			if keywords[strings.ToUpper(t.Text)] {
				vals = append(vals, strings.ToUpper(t.Text))
			} else {
				vals = append(vals, t.Text)
			}
			kinds = append(kinds, TokIdent)
		default:
			vals = append(vals, t.Text)
			kinds = append(kinds, t.Kind)
		}
	}

	vals, kinds = collapseInList(vals, kinds)
	return joinTokens(vals, kinds), nil
}

// collapseInList 把 IN (?, ?, ?) 折叠为 IN (?+)，消除列表长度对指纹的影响。
func collapseInList(vals []string, kinds []TokKind) ([]string, []TokKind) {
	res := make([]string, 0, len(vals))
	rk := make([]TokKind, 0, len(kinds))
	for i := 0; i < len(vals); i++ {
		if kinds[i] == TokIdent && strings.EqualFold(vals[i], "IN") &&
			i+2 < len(vals) && vals[i+1] == "(" && vals[i+2] == "?" {
			j := i + 2
			n := 0
			ok := true
			for j < len(vals) {
				if vals[j] != "?" {
					ok = false
					break
				}
				n++
				j++
				if j < len(vals) && vals[j] == "," {
					j++
					continue
				}
				break
			}
			if ok && n >= 1 && j < len(vals) && vals[j] == ")" {
				res = append(res, vals[i], "(", "?+", ")")
				rk = append(rk, TokIdent, TokPunct, TokParam, TokPunct)
				i = j
				continue
			}
		}
		res = append(res, vals[i])
		rk = append(rk, kinds[i])
	}
	return res, rk
}

func joinTokens(vals []string, kinds []TokKind) string {
	var b strings.Builder
	for i, v := range vals {
		if i > 0 && needSpace(vals[i-1], v) {
			b.WriteByte(' ')
		}
		b.WriteString(v)
	}
	_ = kinds
	return b.String()
}

func needSpace(prev, cur string) bool {
	switch cur {
	case ",", ")", ";", ".", "]":
		return false
	}
	switch prev {
	case "(", ".", "[":
		return false
	}
	return true
}
