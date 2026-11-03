package model

// Namespace 是对象的限定路径。各引擎只使用其中存在的层。
type Namespace struct {
	Catalog string `json:"catalog,omitempty"`
	Schema  string `json:"schema,omitempty"`
}

// ObjectRef 指向一个数据库对象。
type ObjectRef struct {
	Kind    ObjectKind `json:"kind"`
	Catalog string     `json:"catalog,omitempty"`
	Schema  string     `json:"schema,omitempty"`
	Name    string     `json:"name"`
}

// Dependent 是「另一个对象依赖我」的一条记录，用于删除前的阻断。
//
// Exact 的语义很重要：外键依赖三个引擎都能精确回答（true）；视图依赖在
// SQLite / MySQL 5.7 上只能文本匹配（false，可能误报）。宁可多报也不漏报——
// 多报的代价是一次人工确认，漏报的代价是删掉一张被引用的表且没有回滚。
type Dependent struct {
	// Kind 是依赖类型：foreign_key | view。
	Kind string `json:"kind"`
	// Name 是约束名或视图名。
	Name string `json:"name"`
	// Detail 是给人看的一句话，例如 "orders.customer_id → customers.id"。
	Detail string `json:"detail"`
	// Columns 是本对象被引用到的列。外键依赖有值，视图依赖为空
	// （视图引用的列无法可靠判定）。删列时靠它做精确阻断。
	Columns []string `json:"columns,omitempty"`
	// Exact 为 false 时，提示文案必须注明「按文本匹配得到，可能误报」。
	Exact bool `json:"exact"`
}

// Column 是中立列描述。Type.Raw 保留引擎原始类型串。
type Column struct {
	Name        string     `json:"name"`
	Type        ColumnType `json:"type"`
	Nullable    bool       `json:"nullable"`
	Default     *string    `json:"default,omitempty"`
	Comment     string     `json:"comment,omitempty"`
	IsPrimary   bool       `json:"is_primary"`
	IsUnique    bool       `json:"is_unique"`
	IsGenerated bool       `json:"is_generated"`
	Ordinal     int        `json:"ordinal"`
}

// Index 是索引描述。
type Index struct {
	Name      string   `json:"name"`
	Columns   []string `json:"columns"`
	Unique    bool     `json:"unique"`
	Primary   bool     `json:"primary"`
	Method    string   `json:"method,omitempty"`
	Predicate string   `json:"predicate,omitempty"`
}

// ForeignKey 是外键描述。
type ForeignKey struct {
	Name       string   `json:"name,omitempty"`
	Columns    []string `json:"columns"`
	RefSchema  string   `json:"ref_schema,omitempty"`
	RefTable   string   `json:"ref_table"`
	RefColumns []string `json:"ref_columns"`
	OnDelete   string   `json:"on_delete,omitempty"`
	OnUpdate   string   `json:"on_update,omitempty"`
}

// Check 是检查约束。
type Check struct {
	Name string `json:"name,omitempty"`
	Expr string `json:"expr"`
}

// TableDetail 是表/视图的完整描述。
type TableDetail struct {
	Ref         ObjectRef      `json:"ref"`
	Columns     []Column       `json:"columns"`
	Indexes     []Index        `json:"indexes"`
	ForeignKeys []ForeignKey   `json:"foreign_keys"`
	Checks      []Check        `json:"checks"`
	Comment     string         `json:"comment,omitempty"`
	RowEstimate int64          `json:"row_estimate"`
	SizeBytes   int64          `json:"size_bytes"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// PrimaryKeyColumns 返回主键列名，按 Ordinal 排序。
func (t *TableDetail) PrimaryKeyColumns() []string {
	type pk struct {
		ord  int
		name string
	}
	var pks []pk
	for i, c := range t.Columns {
		if c.IsPrimary {
			ord := c.Ordinal
			if ord == 0 {
				ord = i
			}
			pks = append(pks, pk{ord, c.Name})
		}
	}
	for i := 1; i < len(pks); i++ {
		for j := i; j > 0 && pks[j].ord < pks[j-1].ord; j-- {
			pks[j], pks[j-1] = pks[j-1], pks[j]
		}
	}
	out := make([]string, len(pks))
	for i, p := range pks {
		out[i] = p.name
	}
	return out
}

// ObjectStats 是对象体量统计。
type ObjectStats struct {
	RowEstimate int64 `json:"row_estimate"`
	SizeBytes   int64 `json:"size_bytes"`
}
