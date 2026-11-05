package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/driver/dialect"
	"github.com/maintell/DBRover/internal/model"
)

var (
	_ driver.Connector = (*Connector)(nil)
	_ driver.Metadata  = (*Connector)(nil)
)

// Meta 返回元数据能力。
func (c *Connector) Meta() driver.Metadata { return c }

func (c *Connector) handle() (*sql.DB, error) {
	c.mu.RLock()
	db := c.db
	c.mu.RUnlock()
	if db == nil {
		return nil, fmt.Errorf("sqlite: 连接未打开")
	}
	return db, nil
}

// NeutralType 按 SQLite 的类型亲和性（affinity）规则映射到中立类型。
// 顺序不可调整：POINT 含 INT，按官方规则应归为 INTEGER。
func NeutralType(raw string) model.ColumnType {
	ct := model.ColumnType{Raw: raw}
	u := strings.ToUpper(strings.TrimSpace(raw))

	switch {
	case u == "BOOLEAN" || u == "BOOL":
		ct.Kind = model.TypeBoolean
	case u == "DATE":
		ct.Kind = model.TypeDate
	case strings.HasPrefix(u, "DATETIME"), strings.HasPrefix(u, "TIMESTAMP"):
		ct.Kind = model.TypeTimestamp
	case strings.Contains(u, "INT"):
		ct.Kind = model.TypeInteger
		ct.Bits = 64
	case strings.Contains(u, "CHAR"), strings.Contains(u, "CLOB"), strings.Contains(u, "TEXT"):
		ct.Kind = model.TypeString
	case strings.Contains(u, "BLOB"), u == "":
		ct.Kind = model.TypeBinary
	case strings.Contains(u, "REAL"), strings.Contains(u, "FLOA"), strings.Contains(u, "DOUB"):
		ct.Kind = model.TypeFloat
		ct.Bits = 64
	default:
		ct.Kind = model.TypeDecimal
	}
	// SQLite 不做长度检查，但声明里写了 VARCHAR(64) / DECIMAL(9,3) 就是用户对
	// 形状的意思表示：迁移到会检查长度的目标时必须能看见它，否则「收窄」永远
	// 判不出来（这正是它从前一律 review 的原因之一）。
	if a, b, ok := model.TypeArgs(raw); ok {
		switch ct.Kind {
		case model.TypeString:
			ct.MaxLen = a
		case model.TypeDecimal:
			ct.Precision, ct.Scale = a, b
		}
	}
	return ct
}

// Catalogs 返回数据库列表（main / temp / 附加库）。
func (c *Connector) Catalogs(ctx context.Context) ([]string, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	rows, err := h.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var (
			seq      int
			name     string
			filepath sql.NullString
		)
		if err := rows.Scan(&seq, &name, &filepath); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Schemas 返回模式列表。SQLite 没有模式层，固定返回 main，
// 上层依据 Capabilities().Schemas = false 隐藏该层。
func (c *Connector) Schemas(ctx context.Context, catalog string) ([]string, error) {
	if _, err := c.handle(); err != nil {
		return nil, err
	}
	return []string{"main"}, nil
}

// Objects 返回对象清单。
func (c *Connector) Objects(ctx context.Context, catalog, schema string, kinds ...model.ObjectKind) ([]model.ObjectRef, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	qual := sqliteQualifier(catalog)

	var want []string
	for _, k := range kinds {
		switch k {
		case model.KindTable:
			want = append(want, "'table'")
		case model.KindView:
			want = append(want, "'view'")
		case model.KindIndex:
			want = append(want, "'index'")
		case model.KindTrigger:
			want = append(want, "'trigger'")
		}
	}

	q := "SELECT name, type FROM " + qual + ".sqlite_master WHERE name NOT LIKE 'sqlite_%'"
	if len(want) > 0 {
		q += " AND type IN (" + strings.Join(want, ",") + ")"
	}
	q += " ORDER BY type, name"

	rows, err := h.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.ObjectRef
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, err
		}
		kind := model.ObjectKind(typ)
		out = append(out, model.ObjectRef{
			Kind:    kind,
			Catalog: catalog,
			Schema:  schema,
			Name:    name,
		})
	}
	return out, rows.Err()
}

// DescribeTable 返回表的列、索引、外键与行数。
func (c *Connector) DescribeTable(ctx context.Context, ref model.ObjectRef) (*model.TableDetail, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	qual := sqliteQualifier(ref.Catalog)
	name := dialect.For(model.EngineSQLite).QuoteIdent(ref.Name)

	td := &model.TableDetail{Ref: ref}

	if err := c.loadColumns(ctx, h, qual, name, td); err != nil {
		return nil, err
	}
	// PRAGMA table_info 对不存在的表只是返回空结果集，不会报错。
	// 这里必须显式区分：否则上层会把「表不存在」当成「表没有列」。
	if len(td.Columns) == 0 {
		return nil, fmt.Errorf("%w: 表 %s", driver.ErrObjectNotFound, ref.Name)
	}
	if err := c.loadIndexes(ctx, h, qual, name, td); err != nil {
		return nil, err
	}
	if err := c.loadForeignKeys(ctx, h, qual, name, td); err != nil {
		return nil, err
	}

	var n int64
	if err := h.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qual+"."+name).Scan(&n); err == nil {
		td.RowEstimate = n
	}
	return td, nil
}

// loadColumns 读列定义，**包括生成列**。
//
// 用 `table_xinfo` 而不是 `table_info`：后者不返回生成列（`GENERATED ALWAYS AS …`），
// 于是「列清单」里少了它们——结构页看不到，导出/迁移也不知道它们存在，等于静默漏列。
//
// 第 7 列 hidden 的取值：0 普通列、1 虚拟表的隐藏列（不该出现给用户）、2 生成列（虚拟）、
// 3 生成列（存储）。只跳过 1，2/3 收下并标 IsGenerated——**能不能写**由调用方决定
// （导出/迁移的 INSERT 列清单要跳过它们，读路径照常包含）。
func (c *Connector) loadColumns(ctx context.Context, h *sql.DB, qual, name string, td *model.TableDetail) error {
	rows, err := h.QueryContext(ctx, "PRAGMA "+qual+".table_xinfo("+name+")")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			colName    string
			colType    string
			notNull    int
			defaultVal sql.NullString
			pk         int
			hidden     int
		)
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultVal, &pk, &hidden); err != nil {
			return err
		}
		if hidden == 1 {
			// 虚拟表的隐藏列：它们不是用户定义的结构，列出来只会让人困惑。
			continue
		}
		col := model.Column{
			Name:        colName,
			Type:        NeutralType(colType),
			Nullable:    notNull == 0 && pk == 0,
			IsPrimary:   pk > 0,
			IsGenerated: hidden == 2 || hidden == 3,
			Ordinal:     cid,
		}
		if defaultVal.Valid && !col.IsGenerated {
			v := defaultVal.String
			col.Default = &v
		}
		td.Columns = append(td.Columns, col)
	}
	return rows.Err()
}

func (c *Connector) loadIndexes(ctx context.Context, h *sql.DB, qual, name string, td *model.TableDetail) error {
	rows, err := h.QueryContext(ctx, "PRAGMA "+qual+".index_list("+name+")")
	if err != nil {
		return err
	}
	type rawIdx struct {
		name    string
		unique  int
		origin  string
		partial int
	}
	var raws []rawIdx
	for rows.Next() {
		var (
			seq     int
			idxName string
			unique  int
			origin  string
			partial int
		)
		if err := rows.Scan(&seq, &idxName, &unique, &origin, &partial); err != nil {
			_ = rows.Close()
			return err
		}
		raws = append(raws, rawIdx{name: idxName, unique: unique, origin: origin, partial: partial})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, r := range raws {
		idx := model.Index{
			Name:    r.name,
			Unique:  r.unique != 0,
			Primary: r.origin == "pk",
			// origin == "u" 是 UNIQUE 约束自动建的索引（sqlite_autoindex_*）：
			// 它随建表语句一起重建，导出时不能再补一条同名 CREATE INDEX。
			Constraint: r.origin == "u",
		}
		crows, err := h.QueryContext(ctx, "PRAGMA "+qual+".index_info("+dialect.For(model.EngineSQLite).QuoteIdent(r.name)+")")
		if err != nil {
			return err
		}
		for crows.Next() {
			var (
				seqno int
				cid   int
				col   sql.NullString
			)
			if err := crows.Scan(&seqno, &cid, &col); err != nil {
				_ = crows.Close()
				return err
			}
			if col.Valid {
				idx.Columns = append(idx.Columns, col.String)
			}
		}
		if err := crows.Err(); err != nil {
			_ = crows.Close()
			return err
		}
		_ = crows.Close()
		td.Indexes = append(td.Indexes, idx)
	}
	return nil
}

func (c *Connector) loadForeignKeys(ctx context.Context, h *sql.DB, qual, name string, td *model.TableDetail) error {
	rows, err := h.QueryContext(ctx, "PRAGMA "+qual+".foreign_key_list("+name+")")
	if err != nil {
		return err
	}
	defer rows.Close()

	byID := map[int]*model.ForeignKey{}
	var order []int
	for rows.Next() {
		var (
			id       int
			seq      int
			refTable string
			from     string
			to       sql.NullString
			onUpdate string
			onDelete string
			match    string
		)
		if err := rows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return err
		}
		fk, ok := byID[id]
		if !ok {
			fk = &model.ForeignKey{Name: fmt.Sprintf("fk_%s_%d", name, id), RefTable: refTable, OnDelete: onDelete, OnUpdate: onUpdate}
			byID[id] = fk
			order = append(order, id)
		}
		fk.Columns = append(fk.Columns, from)
		if to.Valid {
			fk.RefColumns = append(fk.RefColumns, to.String)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range order {
		td.ForeignKeys = append(td.ForeignKeys, *byID[id])
	}
	return nil
}

// DDL 返回对象原始建表语句。
func (c *Connector) DDL(ctx context.Context, ref model.ObjectRef) (string, error) {
	h, err := c.handle()
	if err != nil {
		return "", err
	}
	qual := sqliteQualifier(ref.Catalog)
	// 触发器/视图的原文也在 sqlite_master 里，只是 type 不同；按 Kind 收窄一下，
	// 免得把同名对象的 DDL 认错（索引与触发器都可能与表重名）。
	q := "SELECT sql FROM " + qual + ".sqlite_master WHERE name = ?"
	switch ref.Kind {
	case model.KindTrigger:
		q += " AND type = 'trigger'"
	case model.KindView:
		q += " AND type = 'view'"
	case model.KindIndex:
		q += " AND type = 'index'"
	}
	var ddl sql.NullString
	err = h.QueryRowContext(ctx, q, ref.Name).Scan(&ddl)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("sqlite: 对象不存在: %s", ref.Name)
	}
	if err != nil {
		return "", err
	}
	if !ddl.Valid || ddl.String == "" {
		return "", fmt.Errorf("sqlite: 对象 %s 没有 DDL（可能是自动索引）", ref.Name)
	}
	return ddl.String, nil
}

// Search 按名称模糊搜索对象。
func (c *Connector) Search(ctx context.Context, pattern string, limit int) ([]model.ObjectRef, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := h.QueryContext(ctx,
		`SELECT name, type FROM main.sqlite_master
		  WHERE name NOT LIKE 'sqlite_%' AND name LIKE ?
		  ORDER BY name LIMIT ?`,
		"%"+pattern+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.ObjectRef
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, err
		}
		out = append(out, model.ObjectRef{Kind: model.ObjectKind(typ), Catalog: "main", Schema: "main", Name: name})
	}
	return out, rows.Err()
}

func sqliteQualifier(catalog string) string {
	if catalog == "" {
		return "main"
	}
	return dialect.For(model.EngineSQLite).QuoteIdent(catalog)
}

// Dependents 返回依赖 ref 的其他对象。
//
// SQLite 没有系统级的依赖视图，两条路都是「自己算」：
//   - 入向外键：逐表跑 PRAGMA foreign_key_list，找 table 指向 ref 的；
//     这是精确的（PRAGMA 给的是真的解析结果）。
//   - 视图：只能拿 sqlite_master.sql 做词边界文本匹配，标 Exact=false。
func (c *Connector) Dependents(ctx context.Context, ref model.ObjectRef) ([]model.Dependent, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	qual := sqliteQualifier(ref.Catalog)

	// 先把所有表与视图列出来，后面两条路都要用。
	type obj struct{ name, kind, sql string }
	var objs []obj
	rows, err := h.QueryContext(ctx,
		"SELECT name, type, COALESCE(sql,'') FROM "+qual+".sqlite_master WHERE type IN ('table','view')")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.name, &o.kind, &o.sql); err != nil {
			_ = rows.Close()
			return nil, err
		}
		objs = append(objs, o)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	var out []model.Dependent

	// 入向外键：逐表扫 PRAGMA。SQLite 的 PRAGMA 只回答「本表引用谁」，
	// 没有反向查询，所以只能全表扫一遍。
	for _, o := range objs {
		if o.kind != "table" {
			continue
		}
		fkRows, err := h.QueryContext(ctx,
			"PRAGMA "+qual+".foreign_key_list("+dialect.For(model.EngineSQLite).QuoteIdent(o.name)+")")
		if err != nil {
			return nil, err
		}
		// 同一组外键会有多行（每列一行），按 id 聚合。
		type agg struct {
			cols, refCols []string
			refTable      string
		}
		byID := map[int64]*agg{}
		var order []int64
		for fkRows.Next() {
			var (
				id, seq        int64
				refTable, from string
				to             sql.NullString
				onUpdate       sql.NullString
				onDelete       sql.NullString
				match          sql.NullString
			)
			if err := fkRows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				_ = fkRows.Close()
				return nil, err
			}
			if !strings.EqualFold(refTable, ref.Name) {
				continue
			}
			a, ok := byID[id]
			if !ok {
				a = &agg{refTable: refTable}
				byID[id] = a
				order = append(order, id)
			}
			a.cols = append(a.cols, from)
			if to.Valid {
				a.refCols = append(a.refCols, to.String)
			}
		}
		if err := fkRows.Err(); err != nil {
			_ = fkRows.Close()
			return nil, err
		}
		_ = fkRows.Close()

		for _, id := range order {
			a := byID[id]
			out = append(out, model.Dependent{
				Kind: "foreign_key",
				Name: fmt.Sprintf("fk_%s_%d", o.name, id),
				Detail: fmt.Sprintf("%s(%s) → %s(%s)", o.name,
					strings.Join(a.cols, ", "), a.refTable, strings.Join(a.refCols, ", ")),
				Columns: a.refCols,
				Exact:   true,
			})
		}
	}

	// 视图依赖：文本匹配，标 Exact=false。
	for _, o := range objs {
		if o.kind != "view" {
			continue
		}
		if driver.ViewReferencesTable(o.sql, ref.Name) {
			out = append(out, model.Dependent{
				Kind:   "view",
				Name:   o.name,
				Detail: "视图定义里出现了 " + ref.Name,
				Exact:  false,
			})
		}
	}
	return out, nil
}

// Blockers 在 SQLite 上恒为空。
//
// SQLite 是单写者模型：写事务期间其他写会直接收到 SQLITE_BUSY，
// 不存在「某个会话开着事务把 DDL 卡住」这回事，也就没有可探测的东西。
// 返回空列表而不是错误——这是「没事」，不是「查不了」。
func (c *Connector) Blockers(ctx context.Context, threshold time.Duration) ([]model.Blocker, error) {
	return nil, nil
}
