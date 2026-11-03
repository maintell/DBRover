package service

import "regexp"

// redactPatterns 是「语句里出现的口令」的匹配规则。
//
// 为什么要有它：审计默认记录 SQL 文本（docs/10 §6.3），而改密码类语句
// 的文本里就带着新口令。不脱敏的话，审计文件成了第二个口令仓库——
// 而且是明文、还给所有人看的那种。
//
// 只覆盖「口令是字面量」的写法。PG 的 `ALTER ROLE u PASSWORD $$x$$`、
// 预处理参数之类不在其中——那些要么要在词法层解析 dollar-quote，
// 要么口令根本不在文本里。宁可少覆盖，也不要用一个贪心的正则把正常 SQL 吃掉。
var redactPatterns = []*regexp.Regexp{
	// CREATE USER x IDENTIFIED BY 'secret' / ALTER USER x IDENTIFIED BY 'secret'
	regexp.MustCompile(`(?i)(identified\s+by)\s+'[^']*'`),
	// CREATE USER x PASSWORD 'secret'（MySQL 的旧写法）
	regexp.MustCompile(`(?i)(create\s+user\s+[^\s]+(?:\s+@\s*'[^']*')?\s+password)\s+'[^']*'`),
	// SET PASSWORD FOR u = 'secret' / SET PASSWORD = 'secret'
	regexp.MustCompile(`(?i)(set\s+password(?:\s+for\s+[^\s]+)?\s*=\s*)'[^']*'`),
	// 兜底：任何 password 后面（可带 =）跟一个字符串字面量。
	//
	// 这一条覆盖 ALTER ROLE app PASSWORD 'x'、password = 'x'、
	// 以及连接串里的 password='x'——它们的共同点是「password 这个词直接
	// 领着一个字面量」。放在最后执行：前面的具体写法先匹配掉，这条只收尾。
	regexp.MustCompile(`(?i)(\bpassword\b\s*=?\s*)'[^']*'`),
}

// RedactSQL 把语句里的口令字面量替换成 ***，其余原样保留。
//
// 保留上下文是关键：审计要能回答「谁在什么时候改了哪个账号的口令」，
// 所以只把口令本身抹掉，不动账号名与动词。
func RedactSQL(sqlText string) string {
	out := sqlText
	for _, re := range redactPatterns {
		out = re.ReplaceAllString(out, "$1 '***'")
	}
	return out
}
