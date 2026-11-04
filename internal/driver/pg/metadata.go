package pg

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

var (
	_ driver.Connector = (*Connector)(nil)
	_ driver.Metadata  = (*Connector)(nil)
)

// Meta 返回元数据能力。
func (c *Connector) Meta() driver.Metadata { return c }

// NeutralType 把 PG 的类型名（format_type 输出，可能带修饰）映射到中立类型。
func NeutralType(raw string) model.ColumnType {
	ct := model.ColumnType{Raw: raw}
	u := strings.ToLower(strings.TrimSpace(raw))
	base := u
	if i := strings.IndexByte(base, '('); i > 0 {
		base = base[:i]
	}
	base = strings.TrimSuffix(strings.TrimSpace(base), "[]")

	switch base {
	case "smallint", "int2":
		ct.Kind, ct.Bits = model.TypeInteger, 16
	case "integer", "int", "int4":
		ct.Kind, ct.Bits = model.TypeInteger, 32
	case "bigint", "int8":
		ct.Kind, ct.Bits = model.TypeInteger, 64
	case "numeric", "decimal":
		ct.Kind = model.TypeDecimal
	case "real", "float4":
		ct.Kind, ct.Bits = model.TypeFloat, 32
	case "double precision", "float8":
		ct.Kind, ct.Bits = model.TypeFloat, 64
	case "money":
		ct.Kind = model.TypeDecimal
	case "text", "character varying", "varchar", "character", "char", "citext", "name":
		ct.Kind = model.TypeString
	case "bytea":
		ct.Kind = model.TypeBinary
	case "boolean", "bool":
		ct.Kind = model.TypeBoolean
	case "date":
		ct.Kind = model.TypeDate
	case "time", "time without time zone":
		ct.Kind = model.TypeTime
	case "time with time zone", "timetz":
		ct.Kind, ct.WithTZ = model.TypeTime, true
	case "timestamp", "timestamp without time zone":
		ct.Kind = model.TypeTimestamp
	case "timestamp with time zone", "timestamptz":
		ct.Kind, ct.WithTZ = model.TypeTimestamp, true
	case "interval":
		ct.Kind = model.TypeInterval
	case "uuid":
		ct.Kind = model.TypeUUID
	case "json", "jsonb":
		ct.Kind = model.TypeJSON
	case "inet", "cidr", "macaddr":
		ct.Kind = model.TypeNetwork
	case "point", "line", "polygon", "circle", "box", "path":
		ct.Kind = model.TypeGeometry
	default:
		ct.Kind = model.TypeUnknown
	}

	if strings.HasSuffix(u, "[]") {
		elem := NeutralType(strings.TrimSuffix(u, "[]"))
		ct.Kind = model.TypeArray
		ct.Elem = &elem
	}
	return ct
}

// Catalogs 返回可连接的数据库清单。
func (c *Connector) Catalogs(ctx context.Context) ([]string, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	rows, err := h.QueryContext(ctx,
		`SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname`)
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
		out = append(out, name)
	}
	return out, rows.Err()
}

// Schemas 返回模式清单（跳过系统模式）。
func (c *Connector) Schemas(ctx context.Context, catalog string) ([]string, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	rows, err := h.QueryContext(ctx, `
		SELECT nspname FROM pg_namespace
		 WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
		 ORDER BY nspname`)
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
		out = append(out, name)
	}
	return out, rows.Err()
}

// Objects 返回对象清单。
func (c *Connector) Objects(ctx context.Context, catalog, schema string, kinds ...model.ObjectKind) ([]model.ObjectRef, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	if schema == "" {
		schema = c.config().Schema
	}

	// 触发器不在 pg_class 的对象集里（它在 pg_trigger），单独查一次。
	var triggers []model.ObjectRef
	if wantsKind(kinds, model.KindTrigger) {
		if triggers, err = c.triggerRefs(ctx, schema); err != nil {
			return nil, err
		}
	}

	q := `SELECT c.relname, c.relkind
	        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	       WHERE n.nspname = $1 AND c.relkind = ANY($2)
	       ORDER BY c.relname`
	relkinds := []string{"r", "p", "v", "m"}
	if len(kinds) > 0 {
		relkinds = relkinds[:0]
		for _, k := range kinds {
			switch k {
			case model.KindTable:
				relkinds = append(relkinds, "r", "p")
			case model.KindView:
				relkinds = append(relkinds, "v")
			case model.KindMatView:
				relkinds = append(relkinds, "m")
			case model.KindSequence:
				relkinds = append(relkinds, "S")
			}
		}
		if len(relkinds) == 0 {
			return triggers, nil
		}
	}

	rows, err := h.QueryContext(ctx, q, schema, pgArray(relkinds))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []model.ObjectRef
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			return nil, err
		}
		k := model.KindTable
		switch kind {
		case "v":
			k = model.KindView
		case "m":
			k = model.KindMatView
		case "S":
			k = model.KindSequence
		}
		out = append(out, model.ObjectRef{Kind: k, Catalog: catalog, Schema: schema, Name: name})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return append(out, triggers...), nil
}

// wantsKind 判断调用方是否点名了某类对象（kinds 为空表示「全都要」）。
func wantsKind(kinds []model.ObjectKind, want model.ObjectKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// triggerRefs 列出某个 schema 的用户触发器。
//
// tgisinternal 会把外键约束背后的内部触发器排除掉——不排除的话，每个外键都会
// 冒出两条同名 trigger，看起来像用户自己建的。
func (c *Connector) triggerRefs(ctx context.Context, schema string) ([]model.ObjectRef, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	rows, err := h.QueryContext(ctx,
		`SELECT t.tgname
		   FROM pg_trigger t
		   JOIN pg_class c ON c.oid = t.tgrelid
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND NOT t.tgisinternal
		  ORDER BY t.tgname`, schema)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []model.ObjectRef
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, model.ObjectRef{Kind: model.KindTrigger, Catalog: c.config().Catalog, Schema: schema, Name: name})
	}
	return out, rows.Err()
}

// DescribeTable 返回列、索引、外键与体量。
func (c *Connector) DescribeTable(ctx context.Context, ref model.ObjectRef) (*model.TableDetail, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	schema := ref.Schema
	if schema == "" {
		schema = c.config().Schema
	}
	td := &model.TableDetail{Ref: ref}

	if err := c.loadColumns(ctx, h, schema, ref.Name, td); err != nil {
		return nil, err
	}
	// pg_catalog 查不到表时只是返回空行，不会报错。
	// 必须显式区分，否则上层拿到的是「零列的表」而不是「表不存在」。
	if len(td.Columns) == 0 {
		return nil, fmt.Errorf("%w: 表 %s", driver.ErrObjectNotFound, ref.Name)
	}
	if err := c.loadIndexes(ctx, h, schema, ref.Name, td); err != nil {
		return nil, err
	}
	if err := c.loadForeignKeys(ctx, h, schema, ref.Name, td); err != nil {
		return nil, err
	}

	// 表注释与体量一起取：同一个 pg_class 行，多带一列而已。
	_ = h.QueryRowContext(ctx,
		`SELECT GREATEST(c.reltuples,0)::bigint, pg_total_relation_size(c.oid),
		        COALESCE(obj_description(c.oid,'pg_class'),'')
		   FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		  WHERE n.nspname=$1 AND c.relname=$2`, schema, ref.Name).
		Scan(&td.RowEstimate, &td.SizeBytes, &td.Comment)

	return td, nil
}

func (c *Connector) loadColumns(ctx context.Context, h *sql.DB, schema, table string, td *model.TableDetail) error {
	rows, err := h.QueryContext(ctx, `
		SELECT a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       NOT a.attnotnull,
		       pg_get_expr(d.adbin, d.adrelid),
		       a.attnum,
		       COALESCE(col_description(a.attrelid, a.attnum), ''),
		       (a.attidentity <> '' OR a.attgenerated <> '')
		  FROM pg_attribute a
		  LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		 ORDER BY a.attnum`, schema, table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			name, colType, comment string
			nullable, generated    bool
			def                    sql.NullString
			ordinal                int
		)
		if err := rows.Scan(&name, &colType, &nullable, &def, &ordinal, &comment, &generated); err != nil {
			return err
		}
		col := model.Column{
			Name: name, Type: NeutralType(colType), Nullable: nullable,
			Comment: comment, Ordinal: ordinal, IsGenerated: generated,
		}
		if def.Valid {
			v := def.String
			col.Default = &v
		}
		td.Columns = append(td.Columns, col)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// 主键列标记
	pkRows, err := h.QueryContext(ctx, `
		SELECT a.attname
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY(i.indkey)
		 WHERE n.nspname = $1 AND c.relname = $2 AND i.indisprimary`, schema, table)
	if err != nil {
		return err
	}
	defer func() { _ = pkRows.Close() }()

	pk := map[string]bool{}
	for pkRows.Next() {
		var name string
		if err := pkRows.Scan(&name); err != nil {
			return err
		}
		pk[strings.ToLower(name)] = true
	}
	for i := range td.Columns {
		if pk[strings.ToLower(td.Columns[i].Name)] {
			td.Columns[i].IsPrimary = true
		}
	}
	return pkRows.Err()
}

func (c *Connector) loadIndexes(ctx context.Context, h *sql.DB, schema, table string, td *model.TableDetail) error {
	rows, err := h.QueryContext(ctx, `
		SELECT i.relname, ix.indisunique, ix.indisprimary, am.amname,
		       pg_get_indexdef(ix.indexrelid),
		       COALESCE(pg_get_expr(ix.indpred, ix.indrelid), ''),
		       EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = ix.indexrelid)
		  FROM pg_index ix
		  JOIN pg_class i ON i.oid = ix.indexrelid
		  JOIN pg_class t ON t.oid = ix.indrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace
		  JOIN pg_am am ON am.oid = i.relam
		 WHERE n.nspname = $1 AND t.relname = $2
		 ORDER BY i.relname`, schema, table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			name, method, def, pred string
			unique, primary         bool
			fromConstraint          bool
		)
		if err := rows.Scan(&name, &unique, &primary, &method, &def, &pred, &fromConstraint); err != nil {
			return err
		}
		td.Indexes = append(td.Indexes, model.Index{
			Name: name, Unique: unique, Primary: primary, Method: method, Predicate: pred,
			Columns: indexColumns(def), Constraint: fromConstraint,
		})
	}
	return rows.Err()
}

// indexColumns 从 "CREATE INDEX ... ON t USING btree (a, b)" 里取出列清单。
func indexColumns(def string) []string {
	l := strings.LastIndex(def, "(")
	r := strings.LastIndex(def, ")")
	if l < 0 || r <= l {
		return nil
	}
	parts := strings.Split(def[l+1:r], ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c *Connector) loadForeignKeys(ctx context.Context, h *sql.DB, schema, table string, td *model.TableDetail) error {
	rows, err := h.QueryContext(ctx, `
		SELECT con.conname, pg_get_constraintdef(con.oid)
		  FROM pg_constraint con
		  JOIN pg_class t ON t.oid = con.conrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace
		 WHERE n.nspname = $1 AND t.relname = $2 AND con.contype = 'f'
		 ORDER BY con.conname`, schema, table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return err
		}
		td.ForeignKeys = append(td.ForeignKeys, parseFK(name, def))
	}
	return rows.Err()
}

// parseFK 解析 "FOREIGN KEY (a, b) REFERENCES s.t(x, y)"。
func parseFK(name, def string) model.ForeignKey {
	fk := model.ForeignKey{Name: name}
	d := strings.ToUpper(def)
	if i := strings.Index(d, "FOREIGN KEY ("); i >= 0 {
		rest := def[i+len("FOREIGN KEY ("):]
		if j := strings.IndexByte(rest, ')'); j > 0 {
			for _, c := range strings.Split(rest[:j], ",") {
				fk.Columns = append(fk.Columns, strings.Trim(strings.TrimSpace(c), `"`))
			}
			rest = rest[j+1:]
			if k := strings.Index(strings.ToUpper(rest), "REFERENCES "); k >= 0 {
				ref := rest[k+len("REFERENCES "):]
				if p := strings.IndexByte(ref, '('); p > 0 {
					target := strings.TrimSpace(ref[:p])
					parts := strings.Split(target, ".")
					if len(parts) == 2 {
						fk.RefSchema = strings.Trim(parts[0], `"`)
						fk.RefTable = strings.Trim(parts[1], `"`)
					} else {
						fk.RefTable = strings.Trim(target, `"`)
					}
					if q := strings.IndexByte(ref[p:], ')'); q > 0 {
						for _, c := range strings.Split(ref[p+1:p+q], ",") {
							fk.RefColumns = append(fk.RefColumns, strings.Trim(strings.TrimSpace(c), `"`))
						}
					}
				}
			}
		}
	}
	return fk
}

// DDL 拼装建表语句。PG 没有一键获取 DDL 的接口，
// 只能用 pg_get_constraintdef 等目录函数拼出等价语句。
func (c *Connector) DDL(ctx context.Context, ref model.ObjectRef) (string, error) {
	h, err := c.handle()
	if err != nil {
		return "", err
	}
	schema := ref.Schema
	if schema == "" {
		schema = c.config().Schema
	}

	// 序列：没有「取定义」的内置函数，从 pg_sequences 拼回 CREATE SEQUENCE。
	// 拼出来的必须与原序列等价（起始值、步长、上下界、缓存、循环），否则
	// 「拿定义去别的库重建」会悄悄变成另一个序列。
	if ref.Kind == model.KindSequence {
		var dataType string
		var startValue, minValue, maxValue, incrementBy, cacheSize int64
		var cycle bool
		err := h.QueryRowContext(ctx,
			`SELECT data_type, start_value, min_value, max_value, increment_by, cache_size, cycle
			   FROM pg_sequences WHERE schemaname = $1 AND sequencename = $2`,
			schema, ref.Name).
			Scan(&dataType, &startValue, &minValue, &maxValue, &incrementBy, &cacheSize, &cycle)
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("pg: 序列不存在: %s", ref.Name)
		}
		if err != nil {
			return "", err
		}
		var b strings.Builder
		b.WriteString("CREATE SEQUENCE ")
		b.WriteString(quoteIdent(schema) + "." + quoteIdent(ref.Name))
		if dataType != "" {
			b.WriteString(" AS " + dataType)
		}
		fmt.Fprintf(&b, " INCREMENT BY %d MINVALUE %d MAXVALUE %d START WITH %d CACHE %d",
			incrementBy, minValue, maxValue, startValue, cacheSize)
		if cycle {
			b.WriteString(" CYCLE")
		}
		b.WriteString(";")
		return b.String(), nil
	}

	// 触发器：pg_get_triggerdef 给出 CREATE TRIGGER 的原文（不含尾分号）。
	if ref.Kind == model.KindTrigger {
		var def string
		err := h.QueryRowContext(ctx,
			`SELECT pg_get_triggerdef(t.oid) FROM pg_trigger t
			   JOIN pg_class c ON c.oid = t.tgrelid
			   JOIN pg_namespace n ON n.oid = c.relnamespace
			  WHERE n.nspname = $1 AND t.tgname = $2 AND NOT t.tgisinternal`,
			schema, ref.Name).Scan(&def)
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("pg: 触发器不存在: %s", ref.Name)
		}
		if err != nil {
			return "", err
		}
		return def + ";", nil
	}

	// 视图直接取定义。
	if ref.Kind == model.KindView || ref.Kind == model.KindMatView {
		var def string
		q := `SELECT pg_get_viewdef($1::regclass, true)`
		qualified := quoteIdent(schema) + "." + quoteIdent(ref.Name)
		if err := h.QueryRowContext(ctx, q, qualified).Scan(&def); err != nil {
			return "", err
		}
		kind := "CREATE VIEW"
		if ref.Kind == model.KindMatView {
			kind = "CREATE MATERIALIZED VIEW"
		}
		return fmt.Sprintf("%s %s.%s AS\n%s", kind, quoteIdent(schema), quoteIdent(ref.Name), def), nil
	}

	td, err := c.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Schema: schema, Name: ref.Name})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s.%s (\n", quoteIdent(schema), quoteIdent(ref.Name))
	for i, col := range td.Columns {
		b.WriteString("  " + quoteIdent(col.Name) + " " + col.Type.Raw)
		if col.Default != nil && !col.IsGenerated {
			b.WriteString(" DEFAULT " + *col.Default)
		}
		if !col.Nullable {
			b.WriteString(" NOT NULL")
		}
		if i < len(td.Columns)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	// 约束按目录里的原始定义追加，保证可与原表等价。
	cons, err := c.constraintDefs(ctx, h, schema, ref.Name)
	if err != nil {
		return "", err
	}
	for _, cd := range cons {
		b.WriteString(",\n  CONSTRAINT " + quoteIdent(cd.name) + " " + cd.def)
	}
	b.WriteString("\n)")

	ddl := b.String()
	// 非主键索引单独补，否则回放后索引会丢。
	for _, ix := range td.Indexes {
		if ix.Primary {
			continue
		}
		var def string
		if err := h.QueryRowContext(ctx,
			`SELECT pg_get_indexdef(($1 || '.' || $2)::regclass)`,
			quoteIdent(schema), quoteIdent(ix.Name)).Scan(&def); err == nil {
			ddl += ";\n" + def
		}
	}
	return ddl, nil
}

type constraintDef struct {
	name string
	def  string
}

func (c *Connector) constraintDefs(ctx context.Context, h *sql.DB, schema, table string) ([]constraintDef, error) {
	rows, err := h.QueryContext(ctx, `
		SELECT con.conname, pg_get_constraintdef(con.oid)
		  FROM pg_constraint con
		  JOIN pg_class t ON t.oid = con.conrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace
		 WHERE n.nspname = $1 AND t.relname = $2
		   AND con.contype IN ('p','u','f','c')
		 ORDER BY con.conname`, schema, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []constraintDef
	for rows.Next() {
		var cd constraintDef
		if err := rows.Scan(&cd.name, &cd.def); err != nil {
			return nil, err
		}
		out = append(out, cd)
	}
	return out, rows.Err()
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
	// 名称、表注释、列注释都算命中（契约里 `meta.search` 写的是「名称 + 注释」）。
	// 用 COALESCE 是因为没注释时 obj_description 返回 NULL，ILIKE NULL 永远不成立，
	// 会把「没注释」误当成「不匹配」——那样注释查询就会悄悄退化成只搜名字。
	rows, err := h.QueryContext(ctx, `
		SELECT n.nspname, c.relname, c.relkind
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
		   AND c.relkind IN ('r','p','v','m')
		   AND (c.relname ILIKE $1
		        OR COALESCE(obj_description(c.oid, 'pg_class'), '') ILIKE $1
		        OR EXISTS (SELECT 1 FROM pg_attribute a
		                    WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		                      AND COALESCE(col_description(c.oid, a.attnum), '') ILIKE $1))
		 ORDER BY n.nspname, c.relname
		 LIMIT $2`, "%"+pattern+"%", limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []model.ObjectRef
	for rows.Next() {
		var schema, name, kind string
		if err := rows.Scan(&schema, &name, &kind); err != nil {
			return nil, err
		}
		k := model.KindTable
		switch kind {
		case "v":
			k = model.KindView
		case "m":
			k = model.KindMatView
		}
		out = append(out, model.ObjectRef{Kind: k, Schema: schema, Name: name})
	}
	return out, rows.Err()
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// pgArray 把字符串切片转成 PG 的 text[] 字面量参数。
func pgArray(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return "{" + strings.Join(quoted, ",") + "}"
}

// Dependents 返回依赖 ref 的其他对象。
//
// PostgreSQL 两条路都是精确的：
//   - 入向外键：pg_constraint 的 conkey/confkey 直接给出列对应关系。
//   - 视图：information_schema.view_table_usage 是解析后的依赖，不是文本匹配。
func (c *Connector) Dependents(ctx context.Context, ref model.ObjectRef) ([]model.Dependent, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	schema := ref.Schema
	if schema == "" {
		schema = c.config().Schema
	}
	if schema == "" {
		schema = "public"
	}

	var out []model.Dependent

	fkRows, err := h.QueryContext(ctx, `
		SELECT c.conname, t.relname, a.attname, ra.attname
		FROM pg_constraint c
		JOIN pg_class t      ON t.oid  = c.conrelid
		JOIN pg_class rt     ON rt.oid = c.confrelid
		JOIN pg_namespace n  ON n.oid  = rt.relnamespace
		CROSS JOIN LATERAL unnest(c.conkey, c.confkey) AS k(attnum, refattnum)
		JOIN pg_attribute a  ON a.attrelid  = t.oid  AND a.attnum  = k.attnum
		JOIN pg_attribute ra ON ra.attrelid = rt.oid AND ra.attnum = k.refattnum
		WHERE c.contype = 'f' AND rt.relname = $1 AND n.nspname = $2
		ORDER BY c.conname, k.attnum`, ref.Name, schema)
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
		var name, table, col, refCol string
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
		a.refCols = append(a.refCols, refCol)
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

	viewRows, err := h.QueryContext(ctx, `
		SELECT DISTINCT view_name
		FROM information_schema.view_table_usage
		WHERE table_catalog = current_database() AND table_schema = $1 AND table_name = $2
		ORDER BY view_name`, schema, ref.Name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = viewRows.Close() }()
	for viewRows.Next() {
		var name string
		if err := viewRows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, model.Dependent{
			Kind:   "view",
			Name:   name,
			Detail: "视图 " + name + " 引用了 " + ref.Name,
			Exact:  true,
		})
	}
	return out, viewRows.Err()
}

// Blockers 报告可能让 DDL 卡住的会话。
//
//   - 长事务：pg_stat_activity 里 state='idle in transaction' 且开始得够早的。
//     PostgreSQL 的 DDL 需要 ACCESS EXCLUSIVE 锁，被它挡住时整张表都不能读写。
//   - 锁等待：wait_event_type='Lock' 的会话——说明已经有人在排队了。
func (c *Connector) Blockers(ctx context.Context, threshold time.Duration) ([]model.Blocker, error) {
	h, err := c.handle()
	if err != nil {
		return nil, err
	}
	if threshold <= 0 {
		threshold = 30 * time.Second
	}
	secs := int64(threshold / time.Second)

	rows, err := h.QueryContext(ctx, `
		SELECT pid, state, COALESCE(wait_event_type,''), COALESCE(query,''),
		       GREATEST(0, EXTRACT(EPOCH FROM (now() - state_change))::bigint)
		FROM pg_stat_activity
		WHERE datname = current_database()
		  AND pid <> pg_backend_pid()
		  AND (
		    (state LIKE 'idle in transaction%' AND now() - state_change >= make_interval(secs => $1))
		    OR wait_event_type = 'Lock'
		  )
		ORDER BY 5 DESC
		LIMIT 20`, secs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []model.Blocker
	for rows.Next() {
		var pid int64
		var state, waitType, query string
		var secs2 int64
		if err := rows.Scan(&pid, &state, &waitType, &query, &secs2); err != nil {
			return nil, err
		}
		kind := "idle_in_transaction"
		detail := fmt.Sprintf("会话 %d 已 idle in transaction %d 秒；ACCESS EXCLUSIVE 锁会被它挡住", pid, secs2)
		if waitType == "Lock" {
			kind = "lock_wait"
			detail = fmt.Sprintf("会话 %d 正在等锁 %d 秒；再执行拿锁的 DDL 会让队列更长", pid, secs2)
		}
		out = append(out, model.Blocker{
			Kind: kind, Session: fmt.Sprintf("pid %d", pid), Detail: detail, Seconds: secs2,
		})
	}
	return out, rows.Err()
}
