// Package drivertest 是连接器一致性测试套件。
//
// 新增引擎实现必须通过本套件；能力声明与实现不符视为缺陷。
package drivertest

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

// Config 是套件的输入。
type Config struct {
	Engine model.Engine
	// New 返回一个已打开的连接器。readOnly 为 true 时必须返回只读连接。
	// 每次调用应返回一个新的、空的数据库，便于做 DDL 回放验证。
	New func(readOnly bool) (driver.Connector, error)
	// BinaryType 是二进制列的类型名。默认 BLOB；PostgreSQL 需要设为 BYTEA。
	BinaryType string

	// Trigger 在给定表上建一个触发器并返回它的名字；nil 表示这个引擎的
	// 测试环境不提供建触发器的手段，此时 TriggerListing 用例跳过。
	//
	// 做成钩子而不是写进套件：三条 SQL 的触发器语法完全不同（PG 还要先建
	// 函数），硬编码只会得到一个覆盖不到任何引擎的用例。
	Trigger func(ctx context.Context, s driver.Session, table string) (string, error)

	// Sequence 建一个序列，返回名字与「定义里必须出现的片段」（如 INCREMENT BY 3）。
	//
	// 带上期望片段是因为：只比对「取到的定义 vs 自己回放后的定义」是自证——
	// 少拼一个子句，两次都少，比较照样相等。期望值必须来自建序列的那一方。
	// nil 时 SequenceListing 跳过。
	Sequence func(ctx context.Context, s driver.Session) (name string, expect []string, err error)

	// DropSequence 删除一个序列，供 SequenceListing 做「删掉再用取到的定义
	// 重建」的回放验证。给成钩子是因为各引擎的 DROP 语法不同，套件不猜。
	DropSequence func(ctx context.Context, s driver.Session, name string) error

	// Comment 给表写一段注释。nil 表示这个引擎没有注释（SQLite），此时
	// SearchMatchesComments 跳过。
	Comment func(ctx context.Context, s driver.Session, table, text string) error

	// Type 建一个用户类型（枚举），DropType 删它。nil 表示这个引擎没有用户类型，
	// 此时 TypeListing 跳过。
	Type     func(ctx context.Context, s driver.Session, name string, values []string) error
	DropType func(ctx context.Context, s driver.Session, name string) error

	// Procedure 建一个例程（存储过程），返回名字与「定义里必须出现的片段」。
	// nil 表示这个引擎没有例程（或测试环境不提供建的手段），此时 RoutineListing 跳过。
	//
	// 与 Sequence 同理：期望片段必须由「建它的那一方」给出，否则「取到的定义 vs
	// 回放后的定义」是自证——少拼一个子句，两边都少，比较照样相等。
	Procedure func(ctx context.Context, s driver.Session) (name string, expect []string, err error)

	// DropProcedure 删除例程，供 RoutineListing 做「删掉再用取到的定义重建」的验证。
	DropProcedure func(ctx context.Context, s driver.Session, name string) error

	// SlowQuery 是一条会跑上几秒的查询（"SELECT pg_sleep(3)" / "SELECT SLEEP(3)"），
	// 供 SessionsReported 造一条「正在跑」的会话。空表示这个引擎没有会话概念。
	SlowQuery string

	// Partitions 建一张带分区的表，返回表名与必须出现在分区清单里的名字。
	// nil 表示这个引擎没有分区（SQLite），此时 PartitionsReported 跳过。
	Partitions func(ctx context.Context, s driver.Session) (table string, expect []string, err error)

	// Check 给已有表加一条命名检查约束。nil 表示这个引擎加不了（SQLite 只能建表时写、
	// MySQL 5.7 不强制），此时 ChecksReported 跳过。
	Check func(ctx context.Context, s driver.Session, table, name, expr string) error
}

func (c Config) binaryType() string {
	if c.BinaryType == "" {
		return "BLOB"
	}
	return c.BinaryType
}

// Run 执行全部适用用例。
func Run(t *testing.T, cfg Config) {
	t.Helper()

	t.Run("Health", func(t *testing.T) { testHealth(t, cfg) })
	t.Run("DDLAndMetadata", func(t *testing.T) { testDDLAndMetadata(t, cfg) })
	t.Run("TypeFidelity", func(t *testing.T) { testTypeFidelity(t, cfg) })
	// 中立类型里必须带着声明长度，否则收窄判定形同虚设。
	t.Run("TypeCarriesDeclaredLength", func(t *testing.T) { testTypeCarriesDeclaredLength(t, cfg) })
	t.Run("NullVsEmptyString", func(t *testing.T) { testNullVsEmpty(t, cfg) })
	// 注意：这里只验证「驱动支持键集分页所需的 SQL 形态」。
	// 产品侧的分页语义（生成 SQL、主键游标、复合键元组比较、无主键拒绝）
	// 由 service 层的 TestTableRows* 覆盖 —— 不要在驱动套件里假装测了它们。
	t.Run("KeysetSQLSupported", func(t *testing.T) { testKeysetPagination(t, cfg) })
	t.Run("TransactionRollback", func(t *testing.T) { testTxRollback(t, cfg) })
	t.Run("ReadOnlyRejectsWrite", func(t *testing.T) { testReadOnlyRejectsWrite(t, cfg) })
	t.Run("DescribeMissingObjectErrors", func(t *testing.T) { testDescribeMissingObject(t, cfg) })
	t.Run("DependentsReported", func(t *testing.T) { testDependents(t, cfg) })
	t.Run("BlockersReported", func(t *testing.T) { testBlockers(t, cfg) })
	t.Run("DDLRoundTrip", func(t *testing.T) { testDDLRoundTrip(t, cfg) })
	// 对象类型列表：能力位说支持，就必须真的列得出来（docs/04 §3.2）。
	t.Run("TriggerListing", func(t *testing.T) { testTriggerListing(t, cfg) })
	t.Run("SequenceListing", func(t *testing.T) { testSequenceListing(t, cfg) })
	// 用户类型（枚举 / 域）：能力位说支持就必须真列得出来。
	t.Run("TypeListing", func(t *testing.T) { testTypeListing(t, cfg) })
	// 例程：不但要列得出来、取得到定义，定义还得能回放。
	t.Run("RoutineListing", func(t *testing.T) { testRoutineListing(t, cfg) })
	// 会话列表与终止：声明了 SessionMgmt 就必须真列出、真能终止。
	t.Run("SessionsReported", func(t *testing.T) { testSessionsReported(t, cfg) })
	// 分区信息：真建一张分区表再读详情，顺便确认普通表不会被报出分区。
	t.Run("PartitionsReported", func(t *testing.T) { testPartitionsReported(t, cfg) })
	// 性能面板的计数：声明了 Metrics 就必须真给，且缓存计数只增不减。
	t.Run("MetricsReported", func(t *testing.T) { testMetricsReported(t, cfg) })
	// 检查约束：加一条再读回来（能加不能看，等于没有）。
	t.Run("ChecksReported", func(t *testing.T) { testChecksReported(t, cfg) })
	// 检索契约写的是「名称 + 注释」，注释这一半必须有真库验证。
	t.Run("SearchMatchesComments", func(t *testing.T) { testSearchComments(t, cfg) })
	// 索引有效性：声明了就必须真给（PG 的 indisvalid）。
	t.Run("IndexValidityHonest", func(t *testing.T) { testIndexValidityHonest(t, cfg) })
	// 表详情里的表注释（列注释由 DDLAndMetadata 顺带覆盖）。
	t.Run("DescribeReportsTableComment", func(t *testing.T) { testDescribeReportsTableComment(t, cfg) })
	// 大整数保真：> 2^53 的整数必须字符串化，否则 JSON 编码出去就已经丢了低位。
	t.Run("LargeIntsAsStrings", func(t *testing.T) { testLargeInts(t, cfg) })
}

// testBlockers 锁住「预执行检查能看到我们自己开着的事务」。
//
// 为什么必须测：这条决定了高风险 DDL 会不会在「有人开着事务」的库上
// 一路绿灯。断言方式是自己开一个事务待在里面，再从另一条连接查——比断言
// 「返回空」有意义得多，后者在实现完全没写时也能通过。
func testBlockers(t *testing.T, cfg Config) {
	if cfg.Engine == model.EngineSQLite {
		// SQLite 是单写者模型：写事务会直接 SQLITE_BUSY，不存在「长事务卡住 DDL」。
		t.Skip("SQLite 没有长事务概念，Blockers 恒为空")
	}

	ctx := context.Background()
	// 只用一个连接器：套件给每个连接器分配的是独立数据库（PostgreSQL），
	// 两个连接器之间互相看不见对方的事务。同一个连接器的池里有多条物理连接，
	// 占住一条开事务，Blockers 走另一条去查，才是「同一库里有人开着事务」。
	c := open(t, cfg, false)
	hs := session(t, c)

	// 先确保有一张表可读——MySQL 的 innodb_trx 只登记碰过 InnoDB 表的事务。
	mustExec(t, hs, "CREATE TABLE IF NOT EXISTS dbv_blocker_probe (id INT NOT NULL PRIMARY KEY)")
	mustExec(t, hs, "DELETE FROM dbv_blocker_probe")
	mustExec(t, hs, "INSERT INTO dbv_blocker_probe (id) VALUES (1)")

	if err := hs.Begin(ctx); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// 事务里读一次，确保它真的登记到引擎侧。
	if _, err := hs.Execute(ctx, "SELECT id FROM dbv_blocker_probe", nil); err != nil {
		t.Fatalf("事务内查询: %v", err)
	}

	// 等一会儿让「已经持续」超过阈值，再从池里另一条连接看。
	time.Sleep(1200 * time.Millisecond)

	blockers, err := c.Meta().Blockers(ctx, time.Second)
	if err != nil {
		t.Fatalf("Blockers: %v", err)
	}
	if len(blockers) == 0 {
		t.Fatalf("自己在同一连接池的另一条连接里开着事务，Blockers 却什么都没报；"+
			"这条检查形同虚设（引擎 %s）", cfg.Engine)
	}
	for _, b := range blockers {
		if b.Detail == "" {
			t.Errorf("Blockers 项缺少说明: %+v", b)
		}
		if b.Kind != "idle_in_transaction" && b.Kind != "lock_wait" {
			t.Errorf("未知的 Kind: %q", b.Kind)
		}
	}

	_ = hs.Rollback(ctx)
}

// testDependents 锁住「删除前的依赖查询」：造一张父表、一张引用它的子表、
// 一个引用它的视图，三者都必须被报出来；入向外键那条必须是精确的。
//
// 为什么必须测：这是删表/删列唯一的阻断依据。漏报一次就是删掉一张被引用的表，
// 而且 DROP TABLE 没有回滚（docs/07 §5.1、ADR-0006）。
func testDependents(t *testing.T, cfg Config) {
	c := open(t, cfg, false)
	s := session(t, c)
	ctx := context.Background()

	for _, q := range []string{
		"DROP VIEW IF EXISTS dbv_dep_view",
		"DROP TABLE IF EXISTS dbv_dep_child",
		"DROP TABLE IF EXISTS dbv_dep_parent",
	} {
		_, _ = s.Execute(ctx, q, nil)
	}
	mustExec(t, s, "CREATE TABLE dbv_dep_parent (id INT NOT NULL PRIMARY KEY)")
	mustExec(t, s, "CREATE TABLE dbv_dep_child (id INT NOT NULL PRIMARY KEY, pid INT)")
	mustExec(t, s, "CREATE VIEW dbv_dep_view AS SELECT id FROM dbv_dep_parent")
	t.Cleanup(func() {
		_, _ = s.Execute(ctx, "DROP VIEW IF EXISTS dbv_dep_view", nil)
		_, _ = s.Execute(ctx, "DROP TABLE IF EXISTS dbv_dep_child", nil)
		_, _ = s.Execute(ctx, "DROP TABLE IF EXISTS dbv_dep_parent", nil)
	})

	deps, err := c.Meta().Dependents(ctx, model.ObjectRef{Kind: model.KindTable, Name: "dbv_dep_parent"})
	if err != nil {
		t.Fatalf("Dependents: %v", err)
	}

	var views, fks []model.Dependent
	for _, d := range deps {
		switch d.Kind {
		case "view":
			views = append(views, d)
		case "foreign_key":
			fks = append(fks, d)
		}
	}
	if len(views) == 0 {
		t.Errorf("视图依赖没被报出来: %+v", deps)
	}
	for _, v := range views {
		if v.Name == "" {
			t.Errorf("视图依赖缺少名字: %+v", v)
		}
	}

	// 外键要等加了约束才会出现；加不上的引擎（例如不支持 ALTER TABLE ADD
	// CONSTRAINT 的 SQLite）用能力位判断，而不是假装测过。
	if c.Capabilities().AddForeignKey {
		mustExec(t, s, "ALTER TABLE dbv_dep_child ADD CONSTRAINT fk_dep FOREIGN KEY (pid) REFERENCES dbv_dep_parent (id)")
		deps, err = c.Meta().Dependents(ctx, model.ObjectRef{Kind: model.KindTable, Name: "dbv_dep_parent"})
		if err != nil {
			t.Fatalf("Dependents（加外键后）: %v", err)
		}
		exact := 0
		for _, d := range deps {
			if d.Kind == "foreign_key" && d.Exact {
				exact++
			}
		}
		if exact == 0 {
			t.Errorf("入向外键必须是精确的（Exact=true）且被报出来: %+v", deps)
		}
	}

	// 不存在的对象返回空列表而不是错误：删一个已经被别人删掉的对象，
	// 不该变成一个看不懂的报错。
	got, err := c.Meta().Dependents(ctx, model.ObjectRef{Kind: model.KindTable, Name: "dbv_no_such_table_zzz"})
	if err != nil {
		t.Errorf("不存在的对象不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("不存在的对象不该有依赖: %+v", got)
	}
}

// testDescribeMissingObject 锁住一条契约：描述不存在的对象必须返回错误。
//
// 为什么必须测：PRAGMA table_info 与 information_schema 对不存在的表都只是
// 「查不到行」，一不留神就返回一个「零列的表结构」。上层据此无法区分
// 「表不存在」与「表没有列」，会给出误导性的提示（例如把建表失败说成没有主键）。
func testDescribeMissingObject(t *testing.T, cfg Config) {
	c := open(t, cfg, false)
	ref := model.ObjectRef{Kind: model.KindTable, Name: "dbrover_no_such_table_zzz"}
	td, err := c.Meta().DescribeTable(context.Background(), ref)
	if err == nil {
		t.Fatalf("描述不存在的表应报错，实际返回了 %d 列的结构", len(td.Columns))
	}
}

func open(t *testing.T, cfg Config, readOnly bool) driver.Connector {
	t.Helper()
	c, err := cfg.New(readOnly)
	if err != nil {
		t.Fatalf("New(readOnly=%v): %v", readOnly, err)
	}
	if c == nil {
		t.Fatal("New 返回了 nil 连接器")
	}
	t.Cleanup(func() { _ = c.Close() })
	if got := c.Engine(); got != cfg.Engine {
		t.Fatalf("Engine() = %s, 期望 %s", got, cfg.Engine)
	}
	return c
}

func session(t *testing.T, c driver.Connector) driver.Session {
	t.Helper()
	s, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustExec(t *testing.T, s driver.Session, sqlText string, args ...any) *driver.ExecResult {
	t.Helper()
	res, err := s.Execute(context.Background(), sqlText, args)
	if err != nil {
		t.Fatalf("Execute(%q): %v", sqlText, err)
	}
	return res
}

func queryAll(t *testing.T, s driver.Session, sqlText string, args ...any) [][]any {
	t.Helper()
	cur, err := s.Query(context.Background(), sqlText, args, driver.QueryOptions{})
	if err != nil {
		t.Fatalf("Query(%q): %v", sqlText, err)
	}
	defer func() { _ = cur.Close() }()

	var out [][]any
	for {
		rows, more, err := cur.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, rows...)
		if !more {
			break
		}
	}
	return out
}

func testHealth(t *testing.T, cfg Config) {
	c := open(t, cfg, false)
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Version == "" {
		t.Error("Version 为空")
	}
	if h.Identity.Fingerprint == "" {
		t.Error("服务器身份指纹为空")
	}
	if h.ReadOnly {
		t.Error("读写连接不应报告只读")
	}
}

func testDDLAndMetadata(t *testing.T, cfg Config) {
	c := open(t, cfg, false)
	s := session(t, c)

	mustExec(t, s, `CREATE TABLE t_other (id INTEGER PRIMARY KEY)`)
	// 外键写成表级约束：MySQL 5.7 会解析但丢弃列级 REFERENCES（8.0.16 才支持），
	// 表级写法在 MySQL / PostgreSQL / SQLite 上都能真正建出外键。
	mustExec(t, s, `CREATE TABLE t_meta (
		id INTEGER PRIMARY KEY,
		name VARCHAR(64) NOT NULL,
		amount REAL,
		note VARCHAR(32) DEFAULT 'x',
		ref_id INTEGER,
		CONSTRAINT fk_meta_other FOREIGN KEY (ref_id) REFERENCES t_other(id)
	)`)
	mustExec(t, s, `CREATE INDEX ix_t_meta_name ON t_meta(name)`)

	objs, err := c.Meta().Objects(context.Background(), "", "", model.KindTable)
	if err != nil {
		t.Fatalf("Objects: %v", err)
	}
	found := false
	for _, o := range objs {
		if o.Name == "t_meta" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Objects 未包含 t_meta: %+v", objs)
	}

	td, err := c.Meta().DescribeTable(context.Background(), model.ObjectRef{Kind: model.KindTable, Name: "t_meta"})
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if len(td.Columns) != 5 {
		t.Fatalf("列数 = %d, 期望 5", len(td.Columns))
	}
	if got := td.PrimaryKeyColumns(); len(got) != 1 || got[0] != "id" {
		t.Errorf("主键 = %v, 期望 [id]", got)
	}
	for _, col := range td.Columns {
		if col.Name == "name" && col.Nullable {
			t.Error("name 为 NOT NULL，Nullable 应为 false")
		}
	}
	if len(td.Indexes) == 0 {
		t.Error("索引列表为空")
	}
	if len(td.ForeignKeys) != 1 {
		t.Errorf("外键数 = %d, 期望 1", len(td.ForeignKeys))
	}

	ddl, err := c.Meta().DDL(context.Background(), model.ObjectRef{Kind: model.KindTable, Name: "t_meta"})
	if err != nil {
		t.Fatalf("DDL: %v", err)
	}
	if !strings.Contains(strings.ToUpper(ddl), "CREATE TABLE") {
		t.Errorf("DDL 不含 CREATE TABLE: %q", ddl)
	}
}

func testTypeFidelity(t *testing.T, cfg Config) {
	c := open(t, cfg, false)
	s := session(t, c)

	mustExec(t, s, fmt.Sprintf(`CREATE TABLE t_types (
		id INTEGER PRIMARY KEY,
		big BIGINT,
		txt TEXT,
		real_n REAL,
		blob_n %s,
		nul TEXT
	)`, cfg.binaryType()))
	mustExec(t, s,
		`INSERT INTO t_types (id, big, txt, real_n, blob_n, nul) VALUES (?, ?, ?, ?, ?, ?)`,
		int64(1), int64(9007199254740993), "hello 世界 🚀", 3.5, []byte{0xDE, 0xAD, 0xBE, 0xEF}, nil)

	rows := queryAll(t, s, `SELECT id, big, txt, real_n, blob_n, nul FROM t_types ORDER BY id`)
	if len(rows) != 1 {
		t.Fatalf("行数 = %d, 期望 1", len(rows))
	}
	r := rows[0]

	if got, ok := r[0].(int64); !ok || got != 1 {
		t.Errorf("id = %#v, 期望 int64(1)", r[0])
	}
	// 大整数只能是「精确的整数」或「精确的十进制字符串」这两种形态之一：
	// 一旦变成 float64，低位就已经没了（9007199254740993 → …992）。
	// 我们的读路径统一走字符串（docs/05 §3.1），这里也接受 int64 是为了让
	// 这个用例只锁「不丢精度」，不重复锁「用哪种形态」（后者的专职用例是
	// LargeIntsAsStrings）。
	switch v := r[1].(type) {
	case string:
		if v != "9007199254740993" {
			t.Errorf("big = %q, 期望 \"9007199254740993\"（不能走 float64）", v)
		}
	case int64:
		if v != 9007199254740993 {
			t.Errorf("big = %d, 期望 9007199254740993", v)
		}
	case float64:
		t.Errorf("big = %v（float64）, 大整数不能走浮点：低位已经丢了", v)
	default:
		t.Errorf("big = %#v（%T），期望精确的字符串或整数", r[1], r[1])
	}
	if got, ok := r[2].(string); !ok || got != "hello 世界 🚀" {
		t.Errorf("txt = %#v", r[2])
	}
	if got, ok := r[3].(float64); !ok || got != 3.5 {
		t.Errorf("real_n = %#v", r[3])
	}
	b, ok := r[4].([]byte)
	if !ok || !bytes.Equal(b, []byte{0xDE, 0xAD, 0xBE, 0xEF}) {
		t.Errorf("blob_n = %#v", r[4])
	}
	if r[5] != nil {
		t.Errorf("nul = %#v, 期望 nil", r[5])
	}
}

func testNullVsEmpty(t *testing.T, cfg Config) {
	c := open(t, cfg, false)
	s := session(t, c)

	mustExec(t, s, `CREATE TABLE t_null (id INTEGER PRIMARY KEY, v TEXT)`)
	mustExec(t, s, `INSERT INTO t_null (id, v) VALUES (1, ?)`, "")
	mustExec(t, s, `INSERT INTO t_null (id, v) VALUES (2, ?)`, nil)

	rows := queryAll(t, s, `SELECT id, v FROM t_null ORDER BY id`)
	if len(rows) != 2 {
		t.Fatalf("行数 = %d", len(rows))
	}
	if got, ok := rows[0][1].(string); !ok || got != "" {
		t.Errorf("空串读回 = %#v, 期望 \"\"", rows[0][1])
	}
	if rows[1][1] != nil {
		t.Errorf("NULL 读回 = %#v, 期望 nil", rows[1][1])
	}
}

func testKeysetPagination(t *testing.T, cfg Config) {
	c := open(t, cfg, false)
	s := session(t, c)

	mustExec(t, s, `CREATE TABLE t_page (id INTEGER PRIMARY KEY, v TEXT)`)
	for i := 1; i <= 10; i++ {
		mustExec(t, s, `INSERT INTO t_page (id, v) VALUES (?, ?)`, i, fmt.Sprintf("r%d", i))
	}

	seen := map[int64]bool{}
	var last int64

	for page := 0; page < 10; page++ {
		rows := queryAll(t, s,
			`SELECT id FROM t_page WHERE id > ? ORDER BY id LIMIT 3`, last)
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			id := r[0].(int64)
			if seen[id] {
				t.Fatalf("keyset 分页出现重复行: %d", id)
			}
			seen[id] = true
			last = id
		}
	}

	if len(seen) != 10 {
		t.Errorf("共取到 %d 行, 期望 10", len(seen))
	}
}

func testTxRollback(t *testing.T, cfg Config) {
	c := open(t, cfg, false)
	s := session(t, c)

	mustExec(t, s, `CREATE TABLE t_tx (id INTEGER PRIMARY KEY)`)

	if err := s.Begin(context.Background()); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if !s.InTx() {
		t.Error("InTx 应为 true")
	}
	mustExec(t, s, `INSERT INTO t_tx (id) VALUES (1)`)
	if err := s.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if s.InTx() {
		t.Error("回滚后 InTx 应为 false")
	}

	rows := queryAll(t, s, `SELECT COUNT(*) FROM t_tx`)
	if n, ok := rows[0][0].(int64); !ok || n != 0 {
		t.Errorf("回滚后行数 = %#v, 期望 0", rows[0][0])
	}
}

func testReadOnlyRejectsWrite(t *testing.T, cfg Config) {
	c := open(t, cfg, true)
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.ReadOnly {
		t.Fatal("只读连接未报告 ReadOnly")
	}

	s := session(t, c)
	if _, err := s.Execute(context.Background(), `CREATE TABLE nope (id INTEGER)`, nil); err == nil {
		t.Fatal("只读连接上的 CREATE TABLE 应被引擎拒绝")
	}
}

// testDDLRoundTrip 验证「取到的 DDL 能在新库回放并产生等价结构」。
//
// 只回放表 DDL：索引 DDL 在 MySQL 里不存在（索引内联在表定义中），
// 对索引单独取 DDL 是 SQLite/PG 才有的能力，不属于契约。
// 索引是否随表一起重建，通过 DescribeTable 比对索引名来验证。
func testDDLRoundTrip(t *testing.T, cfg Config) {
	src := open(t, cfg, false)
	ss := session(t, src)

	mustExec(t, ss, `CREATE TABLE t_rt (
		id INTEGER PRIMARY KEY,
		name VARCHAR(64) NOT NULL,
		parent_id INTEGER
	)`)
	mustExec(t, ss, `CREATE INDEX ix_t_rt_name ON t_rt(name)`)

	tableDDL, err := src.Meta().DDL(context.Background(), model.ObjectRef{Kind: model.KindTable, Name: "t_rt"})
	if err != nil {
		t.Fatalf("DDL(table): %v", err)
	}

	dst := open(t, cfg, false)
	ds := session(t, dst)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ds.Execute(ctx, tableDDL, nil); err != nil {
		t.Fatalf("回放表 DDL 失败: %v\nDDL: %s", err, tableDDL)
	}

	orig, err := src.Meta().DescribeTable(context.Background(), model.ObjectRef{Kind: model.KindTable, Name: "t_rt"})
	if err != nil {
		t.Fatalf("DescribeTable(orig): %v", err)
	}
	replayed, err := dst.Meta().DescribeTable(context.Background(), model.ObjectRef{Kind: model.KindTable, Name: "t_rt"})
	if err != nil {
		t.Fatalf("DescribeTable(replayed): %v", err)
	}

	if len(orig.Columns) != len(replayed.Columns) {
		t.Fatalf("列数不一致: %d vs %d", len(orig.Columns), len(replayed.Columns))
	}
	for i := range orig.Columns {
		if orig.Columns[i].Name != replayed.Columns[i].Name {
			t.Errorf("列名不一致 [%d]: %s vs %s", i, orig.Columns[i].Name, replayed.Columns[i].Name)
		}
		if orig.Columns[i].Type.Kind != replayed.Columns[i].Type.Kind {
			t.Errorf("列类型不一致 %s: %s vs %s",
				orig.Columns[i].Name, orig.Columns[i].Type.Kind, replayed.Columns[i].Type.Kind)
		}
		if orig.Columns[i].Nullable != replayed.Columns[i].Nullable {
			t.Errorf("可空性不一致 %s", orig.Columns[i].Name)
		}
	}

	// 索引必须随表一起重建（名称可能被引擎规范化，比较列集合）。
	origIdx := map[string]bool{}
	for _, ix := range orig.Indexes {
		origIdx[strings.Join(ix.Columns, ",")] = true
	}
	for _, ix := range replayed.Indexes {
		if !origIdx[strings.Join(ix.Columns, ",")] {
			t.Errorf("回放后出现原表没有的索引: %s(%v)", ix.Name, ix.Columns)
		}
	}
}

// testTypeListing 用引擎自己的钩子建一个真类型，再断言它能被列出来。
//
// 为什么必须真建：EnumPG 声明 UserTypes 之后，对象树里能不能看到类型全靠
// Objects(KindType) 这一条查询。只断言「不报错」在实现完全没写时也能过。
func testTypeListing(t *testing.T, cfg Config) {
	if cfg.Type == nil {
		t.Skip("这个引擎没有用户类型（或没有建类型的手段）")
	}
	ctx := context.Background()
	c := open(t, cfg, false)
	defer func() { _ = c.Close() }()
	s := session(t, c)
	defer func() { _ = s.Close() }()

	const name = "dbv_type_probe"
	if err := cfg.Type(ctx, s, name, []string{"alpha", "beta"}); err != nil {
		t.Fatalf("建类型: %v", err)
	}
	if cfg.DropType != nil {
		defer func() { _ = cfg.DropType(context.Background(), s, name) }()
	}

	objs, err := c.Meta().Objects(ctx, "", "", model.KindType)
	if err != nil {
		t.Fatalf("列类型: %v", err)
	}
	found := false
	for _, o := range objs {
		if o.Kind == model.KindType && o.Name == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("刚建的类型没被列出来：%+v", objs)
	}
}
