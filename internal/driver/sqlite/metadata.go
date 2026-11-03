package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

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

func (c *Connector) loadColumns(ctx context.Context, h *sql.DB, qual, name string, td *model.TableDetail) error {
	rows, err := h.QueryContext(ctx, "PRAGMA "+qual+".table_info("+name+")")
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
		)
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultVal, &pk); err != nil {
			return err
		}
		col := model.Column{
			Name:      colName,
			Type:      NeutralType(colType),
			Nullable:  notNull == 0 && pk == 0,
			IsPrimary: pk > 0,
			Ordinal:   cid,
		}
		if defaultVal.Valid {
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
	var ddl sql.NullString
	err = h.QueryRowContext(ctx,
		"SELECT sql FROM "+qual+".sqlite_master WHERE name = ?", ref.Name).Scan(&ddl)
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
