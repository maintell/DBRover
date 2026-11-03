package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

var (
	_ driver.Connector = (*Connector)(nil)
	_ driver.Metadata  = (*Connector)(nil)
)

// systemSchemas 是展示时跳过的库。
var systemSchemas = map[string]bool{
	"information_schema": true,
	"performance_schema": true,
	"mysql":              true,
	"sys":                true,
}

// Meta 返回元数据能力。
func (c *Connector) Meta() driver.Metadata { return c }

// NeutralType 把 MySQL 的 COLUMN_TYPE / DatabaseTypeName 映射到中立类型。
//
// 注意 TINYINT(1) 的歧义：它既可能是布尔也可能是小整数。
// 这里映射为整数——猜成布尔会让数值 2 显示成 true，比显示成 2 更误导。
func NeutralType(raw string) model.ColumnType {
	ct := model.ColumnType{Raw: raw}
	u := strings.ToUpper(strings.TrimSpace(raw))
	if i := strings.IndexByte(u, '('); i > 0 {
		u = u[:i]
	}
	switch u {
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT":
		ct.Kind = model.TypeInteger
		ct.Bits = intBits(u)
	case "DECIMAL", "NUMERIC":
		ct.Kind = model.TypeDecimal
	case "FLOAT", "DOUBLE", "REAL":
		ct.Kind = model.TypeFloat
		ct.Bits = 64
	case "BIT":
		ct.Kind = model.TypeBinary
	case "CHAR", "VARCHAR", "TINYTEXT", "TEXT", "MEDIUMTEXT", "LONGTEXT":
		ct.Kind = model.TypeString
	case "BINARY", "VARBINARY", "TINYBLOB", "BLOB", "MEDIUMBLOB", "LONGBLOB":
		ct.Kind = model.TypeBinary
	case "DATE":
		ct.Kind = model.TypeDate
	case "DATETIME":
		ct.Kind = model.TypeTimestamp
	case "TIMESTAMP":
		ct.Kind = model.TypeTimestamp
		ct.WithTZ = true
	case "TIME":
		ct.Kind = model.TypeTime
	case "YEAR":
		ct.Kind = model.TypeInteger
		ct.Bits = 16
	case "JSON":
		ct.Kind = model.TypeJSON
	case "ENUM":
		ct.Kind = model.TypeEnum
	case "SET":
		ct.Kind = model.TypeEnum
	case "GEOMETRY", "POINT", "LINESTRING", "POLYGON", "MULTIPOINT",
		"MULTILINESTRING", "MULTIPOLYGON", "GEOMETRYCOLLECTION":
		ct.Kind = model.TypeGeometry
	default:
		ct.Kind = model.TypeUnknown
	}
	return ct
}

func intBits(t string) int {
	switch t {
	case "TINYINT":
		return 8
	case "SMALLINT":
		return 16
	case "MEDIUMINT":
		return 24
	case "INT", "INTEGER":
		return 32
	default:
		return 64
	}
}

// Catalogs 返回库清单（跳过系统库）。
func (c *Connector) Catalogs(ctx context.Context) ([]string, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	rows, err := h.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if systemSchemas[strings.ToLower(name)] {
			continue
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Schemas 返回模式列表。
//
// MySQL 的 database 就是 schema，因此只返回一个同名项，
// 由 Capabilities().Schemas = false 让上层不展示这一层。
func (c *Connector) Schemas(ctx context.Context, catalog string) ([]string, error) {
	if catalog == "" {
		catalog = c.config().Catalog
	}
	return []string{catalog}, nil
}

// Objects 返回对象清单。
func (c *Connector) Objects(ctx context.Context, catalog, schema string, kinds ...model.ObjectKind) ([]model.ObjectRef, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	db := catalog
	if db == "" {
		db = c.config().Catalog
	}

	q := `SELECT TABLE_NAME, TABLE_TYPE, COALESCE(TABLE_COMMENT,''),
	             COALESCE(TABLE_ROWS,0), COALESCE(DATA_LENGTH,0)+COALESCE(INDEX_LENGTH,0)
	        FROM information_schema.TABLES
	       WHERE TABLE_SCHEMA = ?`
	args := []any{db}

	var want []string
	for _, k := range kinds {
		switch k {
		case model.KindTable:
			want = append(want, "'BASE TABLE'")
		case model.KindView:
			want = append(want, "'VIEW'")
		}
	}
	if len(want) > 0 {
		q += " AND TABLE_TYPE IN (" + strings.Join(want, ",") + ")"
	}
	q += " ORDER BY TABLE_NAME"

	rows, err := h.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []model.ObjectRef
	for rows.Next() {
		var name, typ, comment string
		var rowEstimate, size int64
		if err := rows.Scan(&name, &typ, &comment, &rowEstimate, &size); err != nil {
			return nil, err
		}
		kind := model.KindTable
		if typ == "VIEW" {
			kind = model.KindView
		}
		out = append(out, model.ObjectRef{Kind: kind, Catalog: db, Schema: db, Name: name})
	}
	return out, rows.Err()
}

// DescribeTable 返回列、索引、外键与体量。
func (c *Connector) DescribeTable(ctx context.Context, ref model.ObjectRef) (*model.TableDetail, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	db := ref.Catalog
	if db == "" {
		db = c.config().Catalog
	}
	td := &model.TableDetail{Ref: ref}

	if err := c.loadColumns(ctx, h, db, ref.Name, td); err != nil {
		return nil, err
	}
	// information_schema 对不存在的表只是查不到行，不会报错。
	// 必须显式区分，否则上层拿到的是「零列的表」而不是「表不存在」。
	if len(td.Columns) == 0 {
		return nil, fmt.Errorf("%w: 表 %s", driver.ErrObjectNotFound, ref.Name)
	}
	if err := c.loadIndexes(ctx, h, db, ref.Name, td); err != nil {
		return nil, err
	}
	if err := c.loadForeignKeys(ctx, h, db, ref.Name, td); err != nil {
		return nil, err
	}
	_ = h.QueryRowContext(ctx,
		`SELECT COALESCE(TABLE_ROWS,0), COALESCE(DATA_LENGTH,0)+COALESCE(INDEX_LENGTH,0)
		   FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?`,
		db, ref.Name).Scan(&td.RowEstimate, &td.SizeBytes)

	return td, nil
}

func (c *Connector) loadColumns(ctx context.Context, h *sql.DB, db, table string, td *model.TableDetail) error {
	rows, err := h.QueryContext(ctx, `
		SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT,
		       COALESCE(COLUMN_COMMENT,''), COLUMN_KEY, EXTRA, ORDINAL_POSITION
		  FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		 ORDER BY ORDINAL_POSITION`, db, table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			name, colType, nullable, key, extra, comment string
			def                                          sql.NullString
			ordinal                                      int
		)
		if err := rows.Scan(&name, &colType, &nullable, &def, &comment, &key, &extra, &ordinal); err != nil {
			return err
		}
		col := model.Column{
			Name:        name,
			Type:        NeutralType(colType),
			Nullable:    nullable == "YES",
			Comment:     comment,
			IsPrimary:   key == "PRI",
			IsUnique:    key == "UNI",
			Ordinal:     ordinal,
			IsGenerated: strings.Contains(strings.ToUpper(extra), "GENERATED"),
		}
		if def.Valid {
			v := def.String
			col.Default = &v
		}
		td.Columns = append(td.Columns, col)
	}
	return rows.Err()
}

func (c *Connector) loadIndexes(ctx context.Context, h *sql.DB, db, table string, td *model.TableDetail) error {
	rows, err := h.QueryContext(ctx, `
		SELECT INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME, INDEX_TYPE
		  FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		 ORDER BY INDEX_NAME, SEQ_IN_INDEX`, db, table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	byName := map[string]*model.Index{}
	var order []string
	for rows.Next() {
		var (
			name, colName, idxType string
			nonUnique, seq         int
		)
		if err := rows.Scan(&name, &nonUnique, &seq, &colName, &idxType); err != nil {
			return err
		}
		idx, ok := byName[name]
		if !ok {
			idx = &model.Index{
				Name:    name,
				Unique:  nonUnique == 0,
				Primary: name == "PRIMARY",
				Method:  idxType,
			}
			byName[name] = idx
			order = append(order, name)
		}
		idx.Columns = append(idx.Columns, colName)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, n := range order {
		td.Indexes = append(td.Indexes, *byName[n])
	}
	return nil
}

func (c *Connector) loadForeignKeys(ctx context.Context, h *sql.DB, db, table string, td *model.TableDetail) error {
	rows, err := h.QueryContext(ctx, `
		SELECT CONSTRAINT_NAME, COLUMN_NAME, REFERENCED_TABLE_SCHEMA,
		       REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME
		  FROM information_schema.KEY_COLUMN_USAGE
		 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND REFERENCED_TABLE_NAME IS NOT NULL
		 ORDER BY CONSTRAINT_NAME, ORDINAL_POSITION`, db, table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	byName := map[string]*model.ForeignKey{}
	var order []string
	for rows.Next() {
		var (
			name, col, refSchema, refTable string
			refCol                         sql.NullString
		)
		if err := rows.Scan(&name, &col, &refSchema, &refTable, &refCol); err != nil {
			return err
		}
		fk, ok := byName[name]
		if !ok {
			fk = &model.ForeignKey{Name: name, RefSchema: refSchema, RefTable: refTable}
			byName[name] = fk
			order = append(order, name)
		}
		fk.Columns = append(fk.Columns, col)
		if refCol.Valid {
			fk.RefColumns = append(fk.RefColumns, refCol.String)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, n := range order {
		td.ForeignKeys = append(td.ForeignKeys, *byName[n])
	}
	return nil
}

// DDL 返回 SHOW CREATE TABLE 的原文。
func (c *Connector) DDL(ctx context.Context, ref model.ObjectRef) (string, error) {
	h, err := c.handle()
	if err != nil {
		return "", err
	}
	db := ref.Catalog
	if db == "" {
		db = c.config().Catalog
	}

	q := "SHOW CREATE TABLE " + quoteIdent(db) + "." + quoteIdent(ref.Name)
	var name, ddl string
	if err := h.QueryRowContext(ctx, q).Scan(&name, &ddl); err != nil {
		// 视图要用 SHOW CREATE VIEW，结果列不同（多 charset/collation）。
		vq := "SHOW CREATE VIEW " + quoteIdent(db) + "." + quoteIdent(ref.Name)
		var vName, vDDL, charset, collation string
		if err2 := h.QueryRowContext(ctx, vq).Scan(&vName, &vDDL, &charset, &collation); err2 == nil {
			return vDDL, nil
		}
		return "", err
	}
	return ddl, nil
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
	db := c.config().Catalog
	rows, err := h.QueryContext(ctx, `
		SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE
		  FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA NOT IN ('information_schema','performance_schema','mysql','sys')
		   AND TABLE_NAME LIKE ?
		 ORDER BY TABLE_SCHEMA, TABLE_NAME
		 LIMIT ?`, "%"+pattern+"%", limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []model.ObjectRef
	for rows.Next() {
		var schema, name, typ string
		if err := rows.Scan(&schema, &name, &typ); err != nil {
			return nil, err
		}
		kind := model.KindTable
		if typ == "VIEW" {
			kind = model.KindView
		}
		if schema != db && db != "" {
			// 搜索范围跨库，仍返回，交由上层按目录分组。
		}
		out = append(out, model.ObjectRef{Kind: kind, Catalog: schema, Schema: schema, Name: name})
	}
	return out, rows.Err()
}

// quoteIdent 用反引号引用标识符，内部反引号双写。
func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// Dependents 返回依赖 ref 的其他对象。
//
//   - 入向外键：information_schema.key_column_usage 是精确的。
//   - 视图：MySQL 5.7 的 view_table_usage 不可靠（经常为空），只能回落
//     到 views.view_definition 做词边界文本匹配，标 Exact=false。
func (c *Connector) Dependents(ctx context.Context, ref model.ObjectRef) ([]model.Dependent, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	db := ref.Catalog
	if db == "" {
		db = c.config().Catalog
	}
	if db == "" {
		return nil, fmt.Errorf("mysql: 未指定库名，无法查询依赖关系")
	}

	var out []model.Dependent

	// 入向外键：按约束名聚合多列。
	fkRows, err := h.QueryContext(ctx, `
		SELECT CONSTRAINT_NAME, TABLE_NAME, COLUMN_NAME, REFERENCED_COLUMN_NAME
		FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = ? AND REFERENCED_TABLE_SCHEMA = ? AND REFERENCED_TABLE_NAME = ?
		ORDER BY CONSTRAINT_NAME, ORDINAL_POSITION`, db, db, ref.Name)
	if err != nil {
		return nil, err
	}
	type agg struct {
		table         string
		cols, refCols []string
	}
	byName := map[string]*agg{}
	var order []string
	for fkRows.Next() {
		var name, table, col string
		var refCol sql.NullString
		if err := fkRows.Scan(&name, &table, &col, &refCol); err != nil {
			_ = fkRows.Close()
			return nil, err
		}
		a, ok := byName[name]
		if !ok {
			a = &agg{table: table}
			byName[name] = a
			order = append(order, name)
		}
		a.cols = append(a.cols, col)
		if refCol.Valid {
			a.refCols = append(a.refCols, refCol.String)
		}
	}
	if err := fkRows.Err(); err != nil {
		_ = fkRows.Close()
		return nil, err
	}
	_ = fkRows.Close()
	for _, name := range order {
		a := byName[name]
		out = append(out, model.Dependent{
			Kind: "foreign_key",
			Name: name,
			Detail: fmt.Sprintf("%s(%s) → %s(%s)", a.table,
				strings.Join(a.cols, ", "), ref.Name, strings.Join(a.refCols, ", ")),
			Columns: a.refCols,
			Exact:   true,
		})
	}

	// 视图依赖：文本匹配。
	viewRows, err := h.QueryContext(ctx,
		"SELECT TABLE_NAME, COALESCE(VIEW_DEFINITION,'') FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ?", db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = viewRows.Close() }()
	for viewRows.Next() {
		var name, def string
		if err := viewRows.Scan(&name, &def); err != nil {
			return nil, err
		}
		if driver.ViewReferencesTable(def, ref.Name) {
			out = append(out, model.Dependent{
				Kind:   "view",
				Name:   name,
				Detail: "视图定义里出现了 " + ref.Name,
				Exact:  false,
			})
		}
	}
	return out, viewRows.Err()
}
