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

// Blocker 是「可能让 DDL 卡住」的一个会话。
//
// 两种性质必须分开：idle_in_transaction 只是「有人开着事务没提交」，
// 在忙碌的库上永远存在，用它阻断会让工具完全不可用；lock_wait 是
// 「已经有人被锁住了」，此时再上一条会拿锁的 DDL 只会让队列更长。
type Blocker struct {
	// Kind 是类型：idle_in_transaction | lock_wait。
	Kind string `json:"kind"`
	// Session 是会话/事务标识（各引擎不同，只用于定位，不保证可跨引擎理解）。
	Session string `json:"session,omitempty"`
	// Detail 是给人看的一句话。
	Detail string `json:"detail"`
	// Seconds 是已经持续的秒数；未知时为 0。
	Seconds int64 `json:"seconds,omitempty"`
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
	// Constraint 表示这个索引是约束（UNIQUE / PRIMARY KEY）的实现，而不是用户
	// 单独建的索引。导出脚本据此决定要不要补 CREATE INDEX：约束已经随建表语句
	// 一起重建，再补一次会撞名。
	Constraint bool `json:"constraint,omitempty"`
	// Valid 是「索引可用」的引擎判定，nil 表示这个引擎没有这个概念。
	//
	// PostgreSQL 有真实用例：CREATE INDEX CONCURRENTLY 失败或中断会留下
	// indisvalid=false 的索引，它照样占空间、照样出现在 \d 里，却完全不参与
	// 查询——唯一索引也不再保证唯一。把它当成「索引在」报出去是误导。
	// MySQL / SQLite 没有这个状态，恒为 nil（不是 false）。
	Valid *bool `json:"valid,omitempty"`
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
	Ref         ObjectRef    `json:"ref"`
	Columns     []Column     `json:"columns"`
	Indexes     []Index      `json:"indexes"`
	ForeignKeys []ForeignKey `json:"foreign_keys"`
	Checks      []Check      `json:"checks"`
	// Partitions 是分区信息（FR-META-03）。空表示没有分区，或这个引擎没有分区概念。
	//
	// 单列一条而不是塞进 Extra：分区是运维要看的东西，界面要能稳定地画出来；
	// 塞进 map[string]any 之后类型与字段名全靠约定，迟早对不上。
	Partitions  []Partition `json:"partitions,omitempty"`
	Comment     string      `json:"comment,omitempty"`
	RowEstimate int64       `json:"row_estimate"`
	SizeBytes   int64       `json:"size_bytes"`
}

// Partition 是一个分区（或分区表的一个子表）。
//
// PG 与 MySQL 的表述方式不同，这里取并集、各自能填多少填多少：
//   - PG：父表用 PARTITION BY 声明方法，子表各自带 FOR VALUES 边界；
//   - MySQL：同一张表内的每个分区带 METHOD 与 VALUES LESS THAN 边界。
type Partition struct {
	// Name 是子表名（PG）或分区名（MySQL）。
	Name string `json:"name"`
	// Method 是分区方法（RANGE / LIST / HASH）；PG 只在父表那一行给。
	Method string `json:"method,omitempty"`
	// Expression 是分区键表达式（PG 的 (n)、MySQL 的 n）。
	Expression string `json:"expression,omitempty"`
	// Bound 是这一行的边界原文（PG 的 FOR VALUES FROM (0) TO (10)、
	// MySQL 的 VALUES LESS THAN (10)）。
	Bound string `json:"bound,omitempty"`
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
