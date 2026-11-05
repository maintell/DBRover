package drivertest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

// testTriggerListing 验证「能力位说支持触发器」时，对象列表真的能列出来。
//
// 这条用例来自一次真实的错：MySQL / PG 声明 Triggers=true，但
// Objects(KindTrigger) 没有分支，调用安静地返回空列表——用户读到的不是
// 「这个工具还不会查」，而是「这张表没有触发器」。
func testTriggerListing(t *testing.T, cfg Config) {
	if cfg.Trigger == nil {
		t.Skip("这个引擎的测试环境没有提供建触发器的钩子")
	}
	ctx := context.Background()
	c, err := cfg.New(false)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = c.Close() }()

	if !c.Capabilities().Triggers {
		t.Fatal("能力位说不支持触发器，却提供了建触发器的钩子：两处必须一致")
	}

	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := s.Execute(ctx, "CREATE TABLE trig_probe (id INTEGER PRIMARY KEY, v VARCHAR(16))", nil); err != nil {
		_ = s.Close()
		t.Fatalf("建表: %v", err)
	}
	name, err := cfg.Trigger(ctx, s, "trig_probe")
	_ = s.Close()
	if err != nil {
		t.Fatalf("建触发器: %v", err)
	}

	objs, err := c.Meta().Objects(ctx, "", "", model.KindTrigger)
	if err != nil {
		t.Fatalf("Objects(KindTrigger): %v", err)
	}
	var ref model.ObjectRef
	for _, o := range objs {
		if o.Name == name {
			ref = o
			if o.Kind != model.KindTrigger {
				t.Errorf("Kind = %q，期望 trigger", o.Kind)
			}
		}
	}
	if ref.Name == "" {
		t.Fatalf("触发器 %s 没出现在列表里：%+v", name, objs)
	}

	// 只能看到名字、看不到它干什么，等于没做完：定义必须取得到。
	ddl, err := c.Meta().DDL(ctx, ref)
	if err != nil {
		t.Fatalf("DDL(%s): %v", name, err)
	}
	if !strings.Contains(strings.ToUpper(ddl), "TRIGGER") || !strings.Contains(ddl, name) {
		t.Fatalf("触发器定义不像 CREATE TRIGGER %s：%q", name, ddl)
	}

	// 按表过滤时不该把触发器混进来：两者是不同对象。
	tables, err := c.Meta().Objects(ctx, "", "", model.KindTable)
	if err != nil {
		t.Fatalf("Objects(KindTable): %v", err)
	}
	for _, o := range tables {
		if o.Name == name {
			t.Fatalf("按表过滤时不该出现触发器 %s", name)
		}
	}
}

// testSequenceListing 同理：序列是独立对象，声明支持就要列得出来。
func testSequenceListing(t *testing.T, cfg Config) {
	if cfg.Sequence == nil {
		t.Skip("这个引擎没有提供建序列的钩子")
	}
	ctx := context.Background()
	c, err := cfg.New(false)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = c.Close() }()

	if !c.Capabilities().Sequences {
		t.Fatal("能力位说不支持序列，却提供了建序列的钩子：两处必须一致")
	}

	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	name, expect, err := cfg.Sequence(ctx, s)
	if err != nil {
		t.Fatalf("建序列: %v", err)
	}

	objs, err := c.Meta().Objects(ctx, "", "", model.KindSequence)
	if err != nil {
		t.Fatalf("Objects(KindSequence): %v", err)
	}
	var ref model.ObjectRef
	for _, o := range objs {
		if o.Name == name {
			ref = o
		}
	}
	if ref.Name == "" {
		t.Fatalf("序列 %s 没出现在列表里：%+v", name, objs)
	}
	if ref.Kind != model.KindSequence {
		t.Errorf("Kind = %q，期望 sequence", ref.Kind)
	}

	// 取到的定义必须能拿来重建：删掉原序列，用 DDL 回放，再列一次必须还在。
	// 只检查字符串像不像，是自欺——拼错一个子句，引擎才会告诉你。
	ddl, err := c.Meta().DDL(ctx, ref)
	if err != nil {
		t.Fatalf("DDL(%s): %v", name, err)
	}
	if up := strings.ToUpper(ddl); !strings.Contains(up, "CREATE SEQUENCE") || !strings.Contains(ddl, name) {
		t.Fatalf("序列定义不像 CREATE SEQUENCE %s：%q", name, ddl)
	}
	// 期望片段来自建序列的那一方：少拼一个子句这里就会红。
	for _, want := range expect {
		if !strings.Contains(strings.ToUpper(ddl), strings.ToUpper(want)) {
			t.Fatalf("定义里缺 %q：%q", want, ddl)
		}
	}
	if cfg.DropSequence == nil {
		return
	}
	if err := cfg.DropSequence(ctx, s, name); err != nil {
		t.Fatalf("删序列: %v", err)
	}
	if _, err := s.Execute(ctx, strings.TrimSuffix(strings.TrimSpace(ddl), ";"), nil); err != nil {
		t.Fatalf("回放定义失败（拼出来的 DDL 不可用）：%v\n%s", err, ddl)
	}
	again, err := c.Meta().Objects(ctx, "", "", model.KindSequence)
	if err != nil {
		t.Fatalf("重建后 Objects: %v", err)
	}
	var rebuilt model.ObjectRef
	for _, o := range again {
		if o.Name == name {
			rebuilt = o
		}
	}
	if rebuilt.Name == "" {
		t.Fatalf("回放后序列 %s 不在列表里", name)
	}
	// 「能建出来」还不够：参数也得回到原样。否则重建出来的是另一个序列，
	// 只是恰好同名列。再取一次定义逐字比对。
	ddl2, err := c.Meta().DDL(ctx, rebuilt)
	if err != nil {
		t.Fatalf("回放后 DDL: %v", err)
	}
	if ddl2 != ddl {
		t.Fatalf("回放后的定义与原定义不一致（参数丢了）：\n原：%s\n后：%s", ddl, ddl2)
	}
}

// testSearchComments 验证 `meta.search` 的「注释」那一半真的有效。
//
// 只搜名字的话，用户按业务含义（「订单未支付」）找表永远找不到——注释就是
// 给这类问题准备的。
func testSearchComments(t *testing.T, cfg Config) {
	if cfg.Comment == nil {
		t.Skip("这个引擎没有表注释")
	}
	ctx := context.Background()
	c, err := cfg.New(false)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = c.Close() }()

	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	const table = "comment_probe"
	if _, err := s.Execute(ctx, "CREATE TABLE "+table+" (id INTEGER PRIMARY KEY)", nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	const token = "dbrover-comment-token-9f2c"
	if err := cfg.Comment(ctx, s, table, token); err != nil {
		t.Fatalf("写注释: %v", err)
	}

	// 用注释里的词搜：必须能找到这张表。
	objs, err := c.Meta().Search(ctx, "comment-token-9f2c", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, o := range objs {
		if o.Name == table {
			found = true
		}
	}
	if !found {
		t.Fatalf("按注释搜索没找到 %s，返回 %+v", table, objs)
	}

	// 反向：搜一个不存在于任何注释的词，不该把这张表带出来。
	objs, err = c.Meta().Search(ctx, "no-such-token-a71b", 20)
	if err != nil {
		t.Fatalf("Search(负向): %v", err)
	}
	for _, o := range objs {
		if o.Name == table {
			t.Fatalf("无关的词不该命中 %s", table)
		}
	}
}

// testDescribeReportsTableComment 验证表详情里能拿到**表**注释。
//
// 为什么单列一条：列注释一直有，表注释却长期缺着——MySQL 的
// information_schema.TABLES.TABLE_COMMENT、PG 的 obj_description 都查得到，
// 只是没接进 DescribeTable。缺了它，表详情看不到说明、迁移搬不过去、
// 比对只能比列注释，而表面上完全不像缺陷（列注释明明在）。
func testDescribeReportsTableComment(t *testing.T, cfg Config) {
	if cfg.Comment == nil {
		t.Skip("这个引擎没有表注释")
	}
	ctx := context.Background()
	c, err := cfg.New(false)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = c.Close() }()

	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	const table = "comment_detail_probe"
	if _, err := s.Execute(ctx, "CREATE TABLE "+table+" (id INTEGER PRIMARY KEY)", nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	const token = "表说明 token-3d51 中文"
	if err := cfg.Comment(ctx, s, table, token); err != nil {
		t.Fatalf("写注释: %v", err)
	}

	detail, err := c.Meta().DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: table})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if detail.Comment != token {
		t.Fatalf("表注释 = %q，期望 %q（说明 DescribeTable 没取表注释）", detail.Comment, token)
	}
}

// testTypeCarriesDeclaredLength 验证中立类型里带着「声明长度/精度」。
//
// 为什么必须有这条：`model.ColumnType` 有 MaxLen/Precision/Scale 三个字段，
// 而三个驱动长期一个都不填——迁移预检里「源长度超过目标上限会截断」那条规则
// 只在把 Column 手工造出来的单测里成立，真库上永远不触发。看起来像有校验，
// 实际是个摆设，这是最坏的一种「有测试」。
func testTypeCarriesDeclaredLength(t *testing.T, cfg Config) {
	ctx := context.Background()
	c, err := cfg.New(false)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = c.Close() }()

	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Execute(ctx, "CREATE TABLE type_len_probe (id INTEGER PRIMARY KEY, v VARCHAR(37), d DECIMAL(9,3))", nil); err != nil {
		t.Fatalf("建表: %v", err)
	}

	detail, err := c.Meta().DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: "type_len_probe"})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	byName := map[string]model.Column{}
	for _, col := range detail.Columns {
		byName[strings.ToLower(col.Name)] = col
	}
	if v := byName["v"]; v.Type.MaxLen != 37 {
		t.Errorf("varchar(37) 的 MaxLen = %d，期望 37（没填就会让「收窄」判定永远不触发）", v.Type.MaxLen)
	}
	if d := byName["d"]; d.Type.Precision != 9 || d.Type.Scale != 3 {
		t.Errorf("decimal(9,3) 的精度/小数位 = %d/%d，期望 9/3", d.Type.Precision, d.Type.Scale)
	}
}

// testIndexValidityHonest 钉住 Index.Valid 的语义：声明了就必须真给，
// 没声明就必须是 nil。
//
// 这个字段存在的理由很具体：PostgreSQL 的 CREATE INDEX CONCURRENTLY 失败或中断
// 会留下 indisvalid=false 的索引——它在 \d 里照样列出来、照样占空间，却完全不
// 参与查询，唯一索引也不再保证唯一。而「有几个索引」这种报法看不出区别。
//
// 两个方向都要断言：不查就报 nil（能力位是 false 的引擎），查了却报 false 说明
// 我们读错了列——刚建好的索引不可能是无效的。
func testIndexValidityHonest(t *testing.T, cfg Config) {
	ctx := context.Background()
	c, err := cfg.New(false)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = c.Close() }()
	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Execute(ctx, "CREATE TABLE idx_valid_probe (id INTEGER PRIMARY KEY, v VARCHAR(20))", nil); err != nil {
		t.Fatalf("建表: %v", err)
	}
	if _, err := s.Execute(ctx, "CREATE INDEX idx_valid_probe_v ON idx_valid_probe (v)", nil); err != nil {
		t.Fatalf("建索引: %v", err)
	}
	detail, err := c.Meta().DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: "idx_valid_probe"})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if len(detail.Indexes) == 0 {
		t.Fatal("一个索引都没读到，后面的断言等于空跑")
	}
	declared := c.Capabilities().IndexValidity
	for _, ix := range detail.Indexes {
		switch {
		case declared && ix.Valid == nil:
			t.Errorf("声明了 IndexValidity，但索引 %s 没有 Valid", ix.Name)
		case declared && !*ix.Valid:
			t.Errorf("刚建好的索引 %s 被报成无效，说明读错了列", ix.Name)
		case !declared && ix.Valid != nil:
			t.Errorf("没有 IndexValidity 能力，索引 %s 不该给出 Valid=%v", ix.Name, *ix.Valid)
		}
	}
}

// testRoutineListing 验证例程这条链路：列得出来、取得到定义、删掉之后立刻消失
// （证明是实时查询而不是缓存），以及**定义能回放**。
//
// 回放这一条是关键：拼出来的语句如果回放不了，「查看定义」就只是一段好看的文字。
// 顺带也验证了切分器认得 `BEGIN … END` 体——套件的钩子建的就是带体的过程。
func testRoutineListing(t *testing.T, cfg Config) {
	if cfg.Procedure == nil {
		t.Skip("这个引擎没有提供建例程的钩子")
	}
	ctx := context.Background()
	c, err := cfg.New(false)
	if err != nil {
		t.Fatalf("连接: %v", err)
	}
	defer func() { _ = c.Close() }()

	if !c.Capabilities().Routines {
		t.Fatal("能力位说不支持例程，却提供了建例程的钩子：两处必须一致")
	}

	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = s.Close() }()

	name, expect, err := cfg.Procedure(ctx, s)
	if err != nil {
		t.Fatalf("建例程: %v", err)
	}
	if cfg.DropProcedure != nil {
		// 最后兜底删掉（包括回放重建出来的那个）；defer 后注册先执行，
		// 所以它跑在 s.Close 之前。
		defer func() { _ = cfg.DropProcedure(context.Background(), s, name) }()
	}

	ref := routineRef(t, c, name, true)
	ddl, err := c.Meta().DDL(ctx, ref)
	if err != nil {
		t.Fatalf("DDL(%s): %v", name, err)
	}
	if up := strings.ToUpper(ddl); !strings.Contains(up, "CREATE") || !strings.Contains(up, "PROCEDURE") {
		t.Fatalf("例程定义不像 CREATE PROCEDURE：%q", ddl)
	}
	for _, frag := range expect {
		if !strings.Contains(strings.ToUpper(ddl), strings.ToUpper(frag)) {
			t.Fatalf("定义里没有 %q：%q", frag, ddl)
		}
	}

	// 删掉后必须立刻消失。
	if err := cfg.DropProcedure(ctx, s, name); err != nil {
		t.Fatalf("删例程: %v", err)
	}
	routineRef(t, c, name, false)

	// 用取到的定义重建：回放不了就说明定义不完整。
	if _, err := s.Execute(ctx, ddl, nil); err != nil {
		t.Fatalf("回放取到的定义失败: %v\n%s", err, ddl)
	}
	routineRef(t, c, name, true)
}

// testChecksReported 验证检查约束能读回来（FR-META-03 的「约束」）。
//
// 这条补的是一个真实缺口：PG 的 DescribeTable 以前根本不填 Checks——「加约束」能用，
// 但已有的约束一条都列不出来。只测「加约束成功」发现不了它。
func testChecksReported(t *testing.T, cfg Config) {
	if cfg.Check == nil {
		t.Skip("这个引擎加不了命名检查约束")
	}
	ctx := context.Background()
	c := open(t, cfg, false)
	s := session(t, c)
	defer func() { _ = s.Close() }()

	mustExec(t, s, "CREATE TABLE dbv_ck (id INT NOT NULL PRIMARY KEY, n INT)")
	if err := cfg.Check(ctx, s, "dbv_ck", "ck_n", "n >= 0"); err != nil {
		t.Fatalf("加约束: %v", err)
	}
	td, err := c.Meta().DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: "dbv_ck"})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	found := false
	for _, ck := range td.Checks {
		if ck.Name == "ck_n" {
			found = true
			if !strings.Contains(ck.Expr, "n >= 0") {
				t.Errorf("约束表达式不对：%q", ck.Expr)
			}
		}
	}
	if !found {
		t.Fatalf("刚加的约束没被读回来：%+v", td.Checks)
	}
}

// testPartitionsReported 验证分区信息（FR-META-03）。
//
// 两个方向都查：分区表必须列出分区（含边界），**普通表不能有分区**——
// MySQL 的 information_schema.PARTITIONS 对未分区的表也会返回一行（分区名为 NULL），
// 少了那个过滤，每张普通表都会被报成「有一个分区」。
func testPartitionsReported(t *testing.T, cfg Config) {
	if cfg.Partitions == nil {
		t.Skip("这个引擎没有分区概念（或没有提供建分区表的钩子）")
	}
	ctx := context.Background()
	c := open(t, cfg, false)
	s := session(t, c)
	defer func() { _ = s.Close() }()

	table, expect, err := cfg.Partitions(ctx, s)
	if err != nil {
		t.Fatalf("建分区表: %v", err)
	}
	td, err := c.Meta().DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: table})
	if err != nil {
		t.Fatalf("DescribeTable(%s): %v", table, err)
	}
	if len(td.Partitions) == 0 {
		t.Fatalf("分区表 %s 没有分区信息", table)
	}
	have := map[string]bool{}
	for _, p := range td.Partitions {
		have[p.Name] = true
	}
	for _, want := range expect {
		if !have[want] {
			t.Errorf("分区清单里缺少 %s：%+v", want, td.Partitions)
		}
	}
	// 至少有一行带边界原文，否则「分区」只是个名字，运维看不出范围。
	withBound := false
	for _, p := range td.Partitions {
		if strings.TrimSpace(p.Bound) != "" {
			withBound = true
		}
	}
	if !withBound {
		t.Errorf("没有一个分区带边界：%+v", td.Partitions)
	}

	// 反向：普通表不能有分区。
	mustExec(t, s, "CREATE TABLE dbv_plain (id INT)")
	plain, err := c.Meta().DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: "dbv_plain"})
	if err != nil {
		t.Fatalf("DescribeTable(dbv_plain): %v", err)
	}
	if len(plain.Partitions) != 0 {
		t.Errorf("普通表不该有分区：%+v", plain.Partitions)
	}
}

// testMetricsReported 验证性能面板的计数真能读到（FR-MON-01）。
//
// 判据不是「有没有数字」，而是三件事：上限是正数、连接数至少含自己这条、
// **缓存计数不会倒退**。第三条最要紧——把两个计数器接反（命中与未命中互换）
// 或取错库（PG 的 pg_stat_database 忘了按 datname 收窄）时，命中率会算成一个
// 看起来合理却完全错误的值，只有单调性还守得住。
func testMetricsReported(t *testing.T, cfg Config) {
	c := open(t, cfg, false)
	if !c.Capabilities().Metrics {
		t.Skip("这个引擎没有性能指标")
	}
	mp, ok := c.(driver.MetricsProvider)
	if !ok {
		t.Fatal("能力位声明了 Metrics，但驱动没有实现 MetricsProvider")
	}
	ctx := context.Background()
	before, err := mp.Counters(ctx)
	if err != nil {
		t.Fatalf("Counters: %v", err)
	}
	if before.MaxConnections <= 0 {
		t.Errorf("连接上限应为正数，实际 %d", before.MaxConnections)
	}
	if before.Connections < 1 {
		t.Errorf("连接数至少含自己这条，实际 %d", before.Connections)
	}
	if before.CollectedAt.IsZero() {
		t.Error("CollectedAt 未填：上层无法判断这份读数有多旧")
	}

	// 造一点真实访存，再看计数器是否只增不减。
	s := session(t, c)
	defer func() { _ = s.Close() }()
	// 先删再建：这条用例会重复跑（同一实例连续两次全量），残留表会让第二次直接红。
	mustExec(t, s, "DROP TABLE IF EXISTS dbv_metrics")
	mustExec(t, s, "CREATE TABLE dbv_metrics (id INT NOT NULL PRIMARY KEY, v VARCHAR(32))")
	for i := 0; i < 20; i++ {
		mustExec(t, s, "INSERT INTO dbv_metrics (id, v) VALUES (?, 'x')", i)
	}
	if rows := queryAll(t, s, "SELECT id FROM dbv_metrics ORDER BY id"); len(rows) != 20 {
		t.Fatalf("准备数据失败：%d 行", len(rows))
	}

	after, err := mp.Counters(ctx)
	if err != nil {
		t.Fatalf("Counters（第二次）: %v", err)
	}
	if after.CacheHits < before.CacheHits {
		t.Errorf("缓存命中计数倒退了：%d -> %d", before.CacheHits, after.CacheHits)
	}
	if after.CacheMisses < before.CacheMisses {
		t.Errorf("缓存未命中计数倒退了：%d -> %d", before.CacheMisses, after.CacheMisses)
	}
	if after.CacheHits+after.CacheMisses == 0 {
		t.Error("跑了一轮读写之后缓存计数仍是 0：缓存指标恐怕取错了地方")
	}
}

// testSessionsReported 验证会话列表与终止（FR-MON-02）。
//
// 做法是造一条「正在跑」的会话（在另一条物理连接上跑一条慢查询），再从会话列表
// 里找到它、终止它，并断言那条查询真的报错。只断言「列表非空」在实现完全没写时
// 也能过——列表里本来就有自己这条（虽然被排除了，但别的测试连接还在）。
func testSessionsReported(t *testing.T, cfg Config) {
	if cfg.SlowQuery == "" {
		t.Skip("这个引擎没有会话概念（或没有提供慢查询钩子）")
	}
	ctx := context.Background()
	c := open(t, cfg, false)
	if !c.Capabilities().SessionMgmt {
		t.Fatal("没声明 SessionMgmt 却提供了慢查询钩子：两处必须一致")
	}
	mp, ok := c.(driver.MonitorProvider)
	if !ok {
		t.Fatal("声明了 SessionMgmt 却没有实现 MonitorProvider：能力位与实现必须一致")
	}

	// 占一条物理连接跑慢查询；它会在引擎侧表现为另一条会话。
	s, err := c.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, qerr := s.Execute(context.Background(), cfg.SlowQuery, nil)
		done <- qerr
	}()

	var found driver.SessionInfo
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		sessions, serr := mp.Sessions(ctx)
		if serr != nil {
			_ = s.Close()
			t.Fatalf("Sessions: %v", serr)
		}
		for _, it := range sessions {
			if !it.Idle && strings.Contains(strings.ToUpper(it.Query), strings.ToUpper(cfg.SlowQuery)) {
				found = it
			}
		}
		if found.ID != "" {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if found.ID == "" {
		_ = s.Close()
		t.Fatal("没在会话列表里看到正在跑的那条慢查询")
	}

	if !c.Capabilities().KillSession {
		_ = s.Close()
		t.Skip("这个引擎声明了不能终止会话")
	}
	if err := mp.KillSession(ctx, found.ID); err != nil {
		_ = s.Close()
		t.Fatalf("KillSession(%s): %v", found.ID, err)
	}
	select {
	case qerr := <-done:
		if qerr == nil {
			t.Error("会话被终止了，那条查询却正常返回——终止没有生效")
		}
	case <-time.After(8 * time.Second):
		t.Error("终止后那条查询仍在跑")
	}
	_ = s.Close()

	// 形态校验：KILL 只能拼语句，非法 id 必须在碰数据库之前就被拒。
	if err := mp.KillSession(ctx, "1; DROP TABLE x"); err == nil {
		t.Error("非数字的会话 id 应当被拒")
	}
}

// routineRef 在例程列表里找（或确认找不到）某个名字。
func routineRef(t *testing.T, c driver.Connector, name string, want bool) model.ObjectRef {
	t.Helper()
	objs, err := c.Meta().Objects(context.Background(), "", "", model.KindRoutine)
	if err != nil {
		t.Fatalf("Objects(KindRoutine): %v", err)
	}
	for _, o := range objs {
		if o.Name != name {
			continue
		}
		if !want {
			t.Fatalf("例程 %s 已被删除，却还在列表里：%+v", name, objs)
		}
		if o.Kind != model.KindRoutine {
			t.Errorf("Kind = %q，期望 routine", o.Kind)
		}
		return o
	}
	if want {
		t.Fatalf("例程 %s 没出现在列表里：%+v", name, objs)
	}
	return model.ObjectRef{}
}
