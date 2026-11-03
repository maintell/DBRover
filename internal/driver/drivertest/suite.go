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
	t.Run("NullVsEmptyString", func(t *testing.T) { testNullVsEmpty(t, cfg) })
	// 注意：这里只验证「驱动支持键集分页所需的 SQL 形态」。
	// 产品侧的分页语义（生成 SQL、主键游标、复合键元组比较、无主键拒绝）
	// 由 service 层的 TestTableRows* 覆盖 —— 不要在驱动套件里假装测了它们。
	t.Run("KeysetSQLSupported", func(t *testing.T) { testKeysetPagination(t, cfg) })
	t.Run("TransactionRollback", func(t *testing.T) { testTxRollback(t, cfg) })
	t.Run("ReadOnlyRejectsWrite", func(t *testing.T) { testReadOnlyRejectsWrite(t, cfg) })
	t.Run("DescribeMissingObjectErrors", func(t *testing.T) { testDescribeMissingObject(t, cfg) })
	t.Run("DDLRoundTrip", func(t *testing.T) { testDDLRoundTrip(t, cfg) })
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
	if got, ok := r[1].(int64); !ok || got != 9007199254740993 {
		t.Errorf("big = %#v, 期望 9007199254740993（不能走 float64）", r[1])
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
