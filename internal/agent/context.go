package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/maintell/DBRover/internal/model"
)

// 检索与压缩的默认上限。
//
// 这些数字不是为了「省 token 省到极致」，而是为了给上下文一个可预期的天花板：
// 一个 2000 表的库全量注入是 50 万 token，而模型每轮都要重读一遍。
const (
	// maxRetrievalTokens 是一次检索最多拿几个词去搜。
	maxRetrievalTokens = 4
	// maxRetrievalObjects 是注入给模型的对象名上限。
	maxRetrievalObjects = 40
	// maxDescribeTables 是附带列定义的表数上限。
	maxDescribeTables = 5
	// defaultContextBudget 是历史消息的字符预算（粗估 3 字符 ≈ 1 token）。
	defaultContextBudget = 24000
	// compactedStubLen 是压缩后每条工具结果保留的字符数。
	compactedStubLen = 200
)

// retrieveContext 按问题里出现的词去检索相关对象，拼成一段给模型的背景。
//
// 三路召回在 docs/11 §4.2 里写了四路（名称、注释、结构邻近、向量）。这里只做
// 名称这一路，而且是**朴素**的：拿问题里的词去 LIKE。理由不是「以后再说」，
// 而是注释检索要 FTS、结构邻近要遍历依赖图、向量要嵌入模型——三者都要额外的
// 索引与依赖，而名称匹配在真实排查里已经能覆盖大部分「我提到的那张表」。
//
// 检索失败不影响对话：拿不到背景就按没有背景继续，不能因为元数据查询失败
// 就把整个任务卡住。
func (r *Runtime) retrieveContext(ctx context.Context, question string) string {
	tokens := questionTokens(question)
	if len(tokens) == 0 {
		return ""
	}

	seen := map[string]model.ObjectRef{}
	for _, tk := range tokens {
		objs, err := r.cat.Search(ctx, tk, 20)
		if err != nil {
			continue
		}
		for _, o := range objs {
			key := strings.ToLower(o.Name)
			if _, ok := seen[key]; !ok {
				seen[key] = o
			}
		}
	}
	if len(seen) == 0 {
		return ""
	}

	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("相关对象（按问题里的词自动检索，不是全库清单）：\n")
	shown := names
	if len(shown) > maxRetrievalObjects {
		shown = shown[:maxRetrievalObjects]
	}
	for _, n := range shown {
		o := seen[n]
		fmt.Fprintf(&b, "- %s %s\n", o.Kind, o.Name)
	}
	if len(names) > len(shown) {
		fmt.Fprintf(&b, "（还有 %d 个未列出，需要时用 meta.search 或 meta.objects）\n", len(names)-len(shown))
	}

	// 第三路召回：结构邻近（docs/11 §4.2）。
	//
	// 为什么值得多花几次元数据查询：问「orders 有多少行」时，真正要改的
	// 往往不是 orders 本身，而是依赖它的视图或子表；只给命中对象，模型就
	// 会在「我发现只有 orders」这条路上走到黑。
	if lines := r.relatedLines(ctx, shown, seen); len(lines) > 0 {
		b.WriteString("这些对象依赖上面的对象：\n")
		for _, l := range lines {
			b.WriteString("- " + l + "\n")
		}
	}

	// 只给前几张表的列定义：列定义是最占地方的部分，给多了等于把预算吃光。
	detail := 0
	for _, n := range shown {
		if detail >= maxDescribeTables {
			break
		}
		o := seen[n]
		td, err := r.cat.DescribeTable(ctx, o)
		if err != nil || td == nil || len(td.Columns) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s 的列：%s\n", o.Name, columnSummary(td))
		detail++
	}
	return b.String()
}

// columnSummary 把列定义压成一行：名字 + 类型 + 关键标记。
func columnSummary(td *model.TableDetail) string {
	parts := make([]string, 0, len(td.Columns))
	for _, c := range td.Columns {
		s := c.Name + " " + c.Type.Raw
		if c.IsPrimary {
			s += " 主键"
		} else if !c.Nullable {
			s += " 非空"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "，")
}

// questionTokens 从问题里挑出可以去检索的词。
//
// 两条规则：
//   - 丢掉常见虚词：拿「帮我看看」「有多少」去搜库，命中的全是噪音；
//   - 中文没有空格，一整句会是一个长串。拿它去 LIKE 一条都匹配不到，所以对
//     长中文串额外给出**前 2~3 个字**作为候选——「订单表有多少条」里的「订单」
//     才是能命中表名的那个。
//
// 这不是分词：真正的分词要么引依赖、要么自己写词典，而这里只需要「可能命中的
// 前缀」。多给几个候选的代价是多查几次元数据，比漏检便宜。
func questionTokens(question string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(v string) bool {
		if len([]rune(v)) < 2 || stopWords[strings.ToLower(v)] {
			return false
		}
		low := strings.ToLower(v)
		if seen[low] {
			return false
		}
		seen[low] = true
		out = append(out, v)
		return len(out) >= maxRetrievalTokens
	}

	for _, raw := range strings.FieldsFunc(question, func(r rune) bool {
		return !(r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' || r >= 0x4e00 && r <= 0x9fff)
	}) {
		t := strings.TrimSpace(raw)
		runes := []rune(t)
		if len(runes) >= 5 && hasCJK(t) {
			// 长中文串：先给短前缀（更可能命中表名），再看整串。
			if add(string(runes[:2])) {
				return out
			}
			if add(string(runes[:3])) {
				return out
			}
			continue
		}
		if add(t) {
			return out
		}
	}
	return out
}

// hasCJK 判断串里有没有汉字。
func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

// stopWords 是「去搜也没意义」的词。
//
// 刻意只放最常见的几个：把词表做大就成了分词器，那是另一个工程，
// 而且误杀一个真表名的代价比多搜一次大得多。
var stopWords = map[string]bool{
	"帮我": true, "看看": true, "查一下": true, "查询": true, "分析": true,
	"有没": true, "多少": true, "怎么": true, "什么": true, "哪些": true,
	"这个": true, "那个": true, "数据": true, "表里": true, "一下": true,
	"select": true, "from": true, "where": true, "order": true, "group": true,
	"limit": true, "count": true, "table": true, "the": true, "and": true,
}

// compactHistory 在历史过长时把最早的工具结果压成一指代。
//
// 为什么只压工具结果、不压对话：用户说过的话和模型的结论都很短，而且丢了
// 就没法追溯「为什么走到这一步」；工具结果才是体积的大头（一张表几万行也会
// 被塞进来，虽然我们已经在工具层截断了，但一轮里几十次调用仍然会堆起来）。
//
// 压缩是**有损**的，所以必须在消息里留一句话说明被压过——否则模型会以为
// 自己看过全部内容，然后基于不完整的信息下结论。
func compactHistory(msgs []Message, budget int) ([]Message, int) {
	if budget <= 0 {
		budget = defaultContextBudget
	}
	total := 0
	for _, m := range msgs {
		total += len(m.Content)
	}
	if total <= budget {
		return msgs, 0
	}

	out := make([]Message, len(msgs))
	copy(out, msgs)
	compacted := 0
	// 从最早的一条开始压：最新几条往往是「刚刚发生的事」，压掉它们最容易让模型重复调用。
	for i := range out {
		if total <= budget {
			break
		}
		if out[i].Role != "tool" || len(out[i].Content) <= compactedStubLen {
			continue
		}
		cut := len(out[i].Content) - compactedStubLen
		out[i].Content = out[i].Content[:compactedStubLen] +
			fmt.Sprintf("\n…（已压缩：省略 %d 字符。需要细节请重新调用该工具）", cut)
		total -= cut
		compacted++
	}
	return out, compacted
}

// relatedLines 列出「依赖命中对象」的东西（视图与外键）。
//
// 只用 Dependents 给出的信息，不做二次猜测：
//   - 视图依赖：Name 就是视图名，直接可用；
//   - 外键依赖：Name 是约束名，真正有用的是 Detail（`orders.customer_id → customers.id`），
//     所以原样带 Detail 而不是硬猜表名。
//
// 只查最前面的三个命中对象：依赖查询是逐个对象一次元数据往返，查太多会把
// 一轮的准备时间从毫秒级拖到百毫秒级。
func (r *Runtime) relatedLines(ctx context.Context, hits []string, seen map[string]model.ObjectRef) []string {
	if len(hits) == 0 {
		return nil
	}
	const (
		maxProbe   = 3
		maxRelated = 8
	)
	var out []string
	seenLine := map[string]bool{}
	for i, name := range hits {
		if i >= maxProbe || len(out) >= maxRelated {
			break
		}
		deps, err := r.cat.Dependents(ctx, seen[name])
		if err != nil {
			continue
		}
		for _, d := range deps {
			if len(out) >= maxRelated {
				break
			}
			var line string
			switch {
			case strings.EqualFold(d.Kind, "view"):
				line = d.Name + "（视图，引用 " + name + "）"
			case d.Detail != "":
				line = d.Detail + "（外键约束 " + d.Name + "）"
			default:
				line = d.Kind + " " + d.Name + "（引用 " + name + "）"
			}
			if !d.Exact && !strings.Contains(line, "可能误报") {
				// 近似结果必须标出来，否则模型会把它当精确事实用。
				line += "（按文本匹配得到，可能误报）"
			}
			if seenLine[line] {
				continue
			}
			seenLine[line] = true
			out = append(out, line)
		}
	}
	return out
}
