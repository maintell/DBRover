package dialect

import (
	"strconv"
	"strings"
)

// Analysis 是对单条语句的轻量结构分析结果。
// 它不做完整语法解析，只提取风险分级所需的事实；解析失败一律保守处理。
type Analysis struct {
	Class  Class
	Verb   string
	Writes bool

	// Object 是 DDL 的操作对象类型（TABLE / INDEX / VIEW / SCHEMA ...）。
	Object string

	HasWhere  bool
	Where     []Token
	Condition Condition
	Limit     int // -1 表示未检测到

	WriteInCTE     bool // CTE 内包含写语句
	SelectInto     bool // SELECT ... INTO <table>
	ForUpdate      bool // SELECT ... FOR UPDATE
	CopyFrom       bool // COPY ... FROM
	Explain        bool
	ExplainAnalyze bool

	TargetTable string
}

// Condition 是 WHERE 子句的轻量分析结果。
//
// 安全约束：本结构只用于「收紧」判定，任何不确定都必须落到更严的一侧。
// 特别地，出现顶层 OR / NOT 时不认定主键命中——`id=1 OR 1=1` 与 `id=1`
// 在语法上一样"有等值"，语义上天差地别。
type Condition struct {
	Present    bool
	AlwaysTrue bool     // 恒真：AND 全部合取项恒真，或 OR 存在恒真析取项
	Equals     []string // 存在等值比较的列（小写）；含 OR/NOT 时为空
	In         []string // 存在 IN 的列（小写）；含 OR/NOT 时为空
	Columns    []string // 出现在谓词左侧的列（小写）
	// HasDisjunction 表示顶层出现 OR 或 NOT。为 true 时 Equals/In 不可信。
	HasDisjunction bool
}

// Analyze 分析一条语句。
func (d impl) Analyze(sqlText string) (Analysis, error) {
	a := Analysis{Limit: -1}

	toks, err := Tokenize(sqlText, d.Syntax())
	if err != nil {
		return a, err
	}
	toks = FilterComments(toks)
	if len(toks) == 0 {
		return a, nil
	}

	toks = stripExplain(toks, &a)
	if len(toks) == 0 {
		return a, nil
	}

	verbIdx, verb := mainVerb(toks)
	a.Verb = verb
	a.Class = classOfVerb(verb)

	switch a.Class {
	case ClassDML, ClassDDL, ClassDCL:
		a.Writes = true
	case ClassUnknown:
		// 无法识别的语句（CALL / EXEC / 引擎私有命令）一律按写处理：
		// 只读连接上不允许「猜它是只读」。
		a.Writes = true
	}
	if a.Class == ClassDDL {
		a.Object = ddlObject(toks, verbIdx)
	}

	// WITH 体内含写语句（如 PG 的 WITH x AS (DELETE ... RETURNING *) SELECT）
	if len(toks) > 0 && toks[0].IsKw("WITH") {
		depth := 0
		for i := 1; i < verbIdx && i < len(toks); i++ {
			switch {
			case toks[i].Text == "(":
				depth++
			case toks[i].Text == ")":
				depth--
			case depth >= 1 && isWriteVerb(toks[i]):
				a.WriteInCTE = true
			}
		}
		if a.WriteInCTE {
			a.Writes = true
		}
	}

	// COPY 的方向决定读写
	if verb == "COPY" {
		if findKeyword(toks, verbIdx+1, "FROM") >= 0 {
			a.CopyFrom = true
			a.Writes = true
		} else {
			a.Class = ClassQuery
			a.Writes = false
		}
	}

	a.TargetTable = extractTable(toks, verbIdx, verb)

	if verb == "SELECT" {
		if into := findKeyword(toks, verbIdx+1, "INTO"); into >= 0 {
			from := findKeyword(toks, verbIdx+1, "FROM")
			if from < 0 || into < from {
				a.SelectInto = true
				a.Writes = true
			}
		}
	}
	if f := findKeyword(toks, verbIdx+1, "FOR"); f >= 0 && f+1 < len(toks) && toks[f+1].IsKw("UPDATE") {
		a.ForUpdate = true
	}

	if wi := findKeyword(toks, verbIdx+1, "WHERE"); wi >= 0 {
		a.HasWhere = true
		end := whereEnd(toks, wi+1)
		a.Where = toks[wi+1 : end]
		a.Condition = analyzeCondition(a.Where)
	}

	a.Limit = detectLimit(toks, verbIdx, verb)

	return a, nil
}

// Classify 只返回语句大类。
func (d impl) Classify(sqlText string) (Class, error) {
	a, err := d.Analyze(sqlText)
	if err != nil {
		return ClassUnknown, err
	}
	return a.Class, nil
}

func stripExplain(toks []Token, a *Analysis) []Token {
	for len(toks) > 0 && (toks[0].IsKw("EXPLAIN") || toks[0].IsKw("DESCRIBE") || toks[0].IsKw("DESC")) {
		a.Explain = true
		toks = toks[1:]
		if len(toks) > 0 && toks[0].Text == "(" {
			if n, ok := skipBalanced(toks, 0); ok {
				toks = toks[n:]
			}
		}
		for len(toks) > 0 {
			switch {
			case toks[0].IsKw("ANALYZE"):
				a.ExplainAnalyze = true
				toks = toks[1:]
			// SQLite 的写法是 EXPLAIN QUERY PLAN <语句>；不认这两个词，
			// mainVerb 会拿到 QUERY，整条语句被判成「无法识别 → 按写处理」，
			// 于是「看执行计划」在只读连接上被拒、在可写连接上要审批。
			case toks[0].IsKw("QUERY"), toks[0].IsKw("PLAN"),
				toks[0].IsKw("VERBOSE"), toks[0].IsKw("FORMAT"), toks[0].IsKw("TRADITIONAL"),
				toks[0].IsKw("TREE"), toks[0].IsKw("JSON"), toks[0].IsKw("EXTENDED"),
				toks[0].Text == "=":
				toks = toks[1:]
			default:
				return toks
			}
		}
	}
	return toks
}

func mainVerb(toks []Token) (int, string) {
	if len(toks) == 0 {
		return 0, ""
	}
	if toks[0].IsKw("WITH") {
		depth := 0
		for i := 1; i < len(toks); i++ {
			switch {
			case toks[i].Text == "(":
				depth++
			case toks[i].Text == ")":
				depth--
			case depth == 0 && isStatementVerb(toks[i]):
				return i, toks[i].Upper()
			}
		}
		return 0, toks[0].Upper()
	}
	return 0, toks[0].Upper()
}

// ddlObject 提取 DDL 的操作对象类型，跳过 UNIQUE / MATERIALIZED / OR REPLACE 等修饰。
func ddlObject(toks []Token, verbIdx int) string {
	modifiers := map[string]bool{
		"UNIQUE": true, "MATERIALIZED": true, "TEMP": true, "TEMPORARY": true,
		"OR": true, "REPLACE": true, "IF": true, "NOT": true, "EXISTS": true,
		"CONCURRENTLY": true, "FOREIGN": true,
	}
	targets := map[string]bool{
		"TABLE": true, "INDEX": true, "VIEW": true, "SCHEMA": true, "DATABASE": true,
		"FUNCTION": true, "PROCEDURE": true, "TRIGGER": true, "SEQUENCE": true,
		"TYPE": true, "DOMAIN": true, "EXTENSION": true, "ROLE": true, "USER": true,
	}
	for i := verbIdx + 1; i < len(toks) && i <= verbIdx+6; i++ {
		u := toks[i].Upper()
		if modifiers[u] {
			continue
		}
		if targets[u] {
			return u
		}
	}
	return ""
}

func isStatementVerb(t Token) bool {
	switch t.Upper() {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "MERGE", "TABLE", "VALUES":
		return true
	}
	return false
}

func isWriteVerb(t Token) bool {
	switch t.Upper() {
	case "INSERT", "UPDATE", "DELETE", "MERGE", "REPLACE", "TRUNCATE":
		return true
	}
	return false
}

func classOfVerb(v string) Class {
	switch v {
	case "SELECT", "SHOW", "VALUES", "TABLE", "PRAGMA", "DESCRIBE", "DESC":
		return ClassQuery
	case "INSERT", "UPDATE", "DELETE", "MERGE", "REPLACE", "COPY", "UPSERT":
		return ClassDML
	case "CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME", "COMMENT":
		return ClassDDL
	case "GRANT", "REVOKE":
		return ClassDCL
	case "BEGIN", "START", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE", "SET", "END":
		return ClassTCL
	default:
		return ClassUnknown
	}
}

// findKeyword 在 depth==0 处查找关键字。
func findKeyword(toks []Token, from int, kw string) int {
	depth := 0
	for i := from; i < len(toks); i++ {
		switch toks[i].Text {
		case "(":
			depth++
			continue
		case ")":
			depth--
			continue
		}
		if depth == 0 && toks[i].IsKw(kw) {
			return i
		}
	}
	return -1
}

func whereEnd(toks []Token, from int) int {
	depth := 0
	for i := from; i < len(toks); i++ {
		t := toks[i]
		switch t.Text {
		case "(":
			depth++
			continue
		case ")":
			depth--
			continue
		}
		if depth != 0 {
			continue
		}
		if t.Kind == TokPunct && t.Text == ";" {
			return i
		}
		switch t.Upper() {
		case "ORDER", "LIMIT", "GROUP", "HAVING", "RETURNING", "OFFSET", "FETCH", "WINDOW", "FOR":
			return i
		}
	}
	return len(toks)
}

func detectLimit(toks []Token, verbIdx int, verb string) int {
	if li := findKeyword(toks, verbIdx+1, "LIMIT"); li >= 0 && li+1 < len(toks) {
		if n, ok := tokenInt(toks[li+1]); ok {
			return n
		}
	}
	if fi := findKeyword(toks, verbIdx+1, "FETCH"); fi >= 0 {
		for i := fi + 1; i < len(toks) && i <= fi+4; i++ {
			if n, ok := tokenInt(toks[i]); ok {
				return n
			}
		}
	}
	if verb == "SELECT" {
		for i := verbIdx + 1; i < len(toks) && i <= verbIdx+5; i++ {
			if toks[i].IsKw("TOP") && i+1 < len(toks) {
				if n, ok := tokenInt(toks[i+1]); ok {
					return n
				}
			}
		}
	}
	return -1
}

func tokenInt(t Token) (int, bool) {
	if t.Kind != TokNumber {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(t.Text))
	if err != nil {
		return 0, false
	}
	return n, true
}

func extractTable(toks []Token, verbIdx int, verb string) string {
	switch verb {
	case "UPDATE":
		var last string
		for i := verbIdx + 1; i < len(toks); i++ {
			t := toks[i]
			if t.IsKw("SET") || t.IsKw("FROM") || t.IsKw("WHERE") {
				break
			}
			if t.IsKw("ONLY") || t.Text == "." {
				continue
			}
			if t.Kind == TokIdent || t.Kind == TokQuoted {
				last = t.Value
				continue
			}
			break
		}
		return last
	case "DELETE":
		if f := findKeyword(toks, verbIdx+1, "FROM"); f >= 0 {
			return nextName(toks, f+1)
		}
	case "INSERT", "REPLACE":
		if f := findKeyword(toks, verbIdx+1, "INTO"); f >= 0 {
			return nextName(toks, f+1)
		}
	case "SELECT":
		if f := findKeyword(toks, verbIdx+1, "FROM"); f >= 0 {
			return nextName(toks, f+1)
		}
	}
	return ""
}

func nextName(toks []Token, i int) string {
	for ; i < len(toks); i++ {
		t := toks[i]
		if t.IsKw("ONLY") {
			continue
		}
		if t.Kind == TokIdent || t.Kind == TokQuoted {
			last := t.Value
			j := i
			for j+2 < len(toks) && toks[j+1].Text == "." &&
				(toks[j+2].Kind == TokIdent || toks[j+2].Kind == TokQuoted) {
				last = toks[j+2].Value
				j += 2
			}
			return last
		}
		return ""
	}
	return ""
}

func analyzeCondition(toks []Token) Condition {
	c := Condition{Present: true}
	if len(toks) == 0 {
		return c
	}

	c.HasDisjunction = hasTopLevel(toks, "OR") || hasTopLevel(toks, "NOT")

	// 恒真判定：AND 需要每个合取项都恒真；OR 只需任一析取项恒真。
	if c.HasDisjunction && hasTopLevel(toks, "OR") {
		for _, d := range splitTopLevel(toks, "OR", nil) {
			if isLiteralTrue(trimParens(d)) {
				c.AlwaysTrue = true
				break
			}
		}
	}
	conj := splitTopLevel(toks, "AND", []string{"BETWEEN"})
	allTrue := len(conj) > 0
	for _, ct := range conj {
		if !isLiteralTrue(trimParens(ct)) {
			allTrue = false
			break
		}
	}
	if allTrue {
		c.AlwaysTrue = true
	}

	// 只要出现顶层 OR/NOT，就不采集等值列：调用方据此拒绝判成「主键命中」。
	if c.HasDisjunction {
		return c
	}

	for _, ct := range conj {
		ct = trimParens(ct)
		if len(ct) == 0 {
			continue
		}
		col, op := leadingPredicate(ct)
		if col == "" {
			continue
		}
		col = strings.ToLower(col)
		switch op {
		case "=":
			c.Equals = append(c.Equals, col)
			c.Columns = append(c.Columns, col)
		case "IN":
			c.In = append(c.In, col)
			c.Columns = append(c.Columns, col)
		case "BETWEEN", ">", ">=", "<", "<=", "LIKE", "IS":
			c.Columns = append(c.Columns, col)
		}
	}
	return c
}

// hasTopLevel 判断顶层（括号外）是否出现某个关键字。
func hasTopLevel(toks []Token, kw string) bool {
	depth := 0
	for _, t := range toks {
		switch t.Text {
		case "(":
			depth++
			continue
		case ")":
			depth--
			continue
		}
		if depth == 0 && t.IsKw(kw) {
			return true
		}
	}
	return false
}

// splitTopLevel 按顶层分隔符切分。skipRepeat 中的关键字会吞掉紧随其后的一个分隔符
// （用于 BETWEEN x AND y 里的 AND）。
func splitTopLevel(toks []Token, sep string, skipRepeat []string) [][]Token {
	var out [][]Token
	depth := 0
	pending := 0
	start := 0
	for i, t := range toks {
		switch t.Text {
		case "(":
			depth++
			continue
		case ")":
			depth--
			continue
		}
		if depth != 0 {
			continue
		}
		for _, kw := range skipRepeat {
			if t.IsKw(kw) {
				pending++
			}
		}
		if t.IsKw(sep) {
			if pending > 0 {
				pending--
				continue
			}
			out = append(out, toks[start:i])
			start = i + 1
		}
	}
	out = append(out, toks[start:])
	return out
}

// isLiteralTrue 只认可两种恒真形式：单独的 TRUE，以及「同一字面量 = 同一字面量」。
//
// 刻意不把「不引用列」当作恒真：`EXISTS (SELECT 1)` 也不引用外层列，
// 但它显然不是恒真。判定宁可漏判（落到更严的等级）也不要误判。
func isLiteralTrue(toks []Token) bool {
	if len(toks) == 1 && toks[0].IsKw("TRUE") {
		return true
	}
	if len(toks) == 3 && toks[1].Kind == TokOp && toks[1].Text == "=" {
		a, b := toks[0], toks[2]
		if isLiteralTok(a) && isLiteralTok(b) && a.Text == b.Text {
			return true
		}
	}
	return false
}

func isLiteralTok(t Token) bool {
	switch t.Kind {
	case TokNumber, TokString, TokParam:
		return true
	}
	return false
}

func trimParens(toks []Token) []Token {
	for len(toks) >= 2 && toks[0].Text == "(" {
		n, ok := skipBalanced(toks, 0)
		if !ok || n != len(toks) {
			break
		}
		toks = toks[1 : len(toks)-1]
	}
	return toks
}

// skipBalanced 返回与 toks[i] 的 '(' 匹配的 ')' 之后的下标。
func skipBalanced(toks []Token, i int) (int, bool) {
	if i >= len(toks) || toks[i].Text != "(" {
		return i, false
	}
	depth := 0
	for j := i; j < len(toks); j++ {
		switch toks[j].Text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return j + 1, true
			}
		}
	}
	return i, false
}

// leadingPredicate 提取合取项开头的「列 运算符」。
func leadingPredicate(ct []Token) (string, string) {
	ct = stripQualifier(ct)
	if len(ct) < 2 {
		return "", ""
	}
	if !isColumnTok(ct[0]) {
		return "", ""
	}
	col := ct[0].Value
	op := ct[1]
	switch {
	case op.Kind == TokOp:
		switch op.Text {
		case "=", ">", ">=", "<", "<=":
			return col, op.Text
		}
	case op.IsKw("IN"):
		return col, "IN"
	case op.IsKw("BETWEEN"):
		return col, "BETWEEN"
	case op.IsKw("LIKE"):
		return col, "LIKE"
	case op.IsKw("IS"):
		return col, "IS"
	}
	return "", ""
}

func stripQualifier(ct []Token) []Token {
	for len(ct) >= 3 &&
		(ct[0].Kind == TokIdent || ct[0].Kind == TokQuoted) &&
		ct[1].Text == "." &&
		(ct[2].Kind == TokIdent || ct[2].Kind == TokQuoted) {
		ct = ct[2:]
	}
	return ct
}

func isColumnTok(t Token) bool {
	switch t.Kind {
	case TokQuoted:
		return true
	case TokIdent:
		return !keywords[t.Upper()]
	}
	return false
}
