package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/maintell/DBRover/internal/driver"
	"github.com/maintell/DBRover/internal/model"
)

// DBR 备份容器（v1）。
//
// 布局（全部小端）：
//
//	magic      8 字节  "DBROVER\\x00"
//	version    u16     当前 1
//	flags      u16     bit0=encrypted（v1 恒 0）、bit1=compressed（v1 恒 0）
//	header_len u32     后面 header JSON 的字节数
//	header     JSON    BackupHeader
//	sections   每段：u32 len + 该表导出的 SQL 脚本文本
//
// v1 刻意只做「能打开、能列出对象、能取出每段脚本」这三件事：
// 续跑、加密、压缩、恢复演练都还没做（见 docs/08 §2 的目标形态）。
const (
	backupMagic         = "DBROVER\x00"
	backupFormatVersion = 1

	// flags 位：bit0 加密（v1 不支持）、bit1 压缩（gzip 每段）。
	backupFlagEncrypted  uint16 = 1 << 0
	backupFlagCompressed uint16 = 1 << 1
)

// BackupObject 是容器头部里的一个对象条目。
//
// v1 不含行数与大小：它们要等表导完才知道，而头必须在最前面。
// 需要行数时请从段里的脚本自行统计，或用「导出统计」接口。
type BackupObject struct {
	Kind   string `json:"kind"`
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
	// Indexes / ForeignKeys 是备份时该对象的索引与外键个数，用来在恢复后比对。
	//
	// 只记数量：名字与定义已经在那段 SQL 里了，重复存一份只会带来不一致。
	// 老容器没有这两个字段（0），恢复时跳过比对而不是报「缺 0 个」。
	Indexes     int `json:"indexes,omitempty"`
	ForeignKeys int `json:"foreign_keys,omitempty"`
}

// BackupHeader 是容器的头部。
type BackupHeader struct {
	FormatVersion int    `json:"format_version"`
	CreatedAt     string `json:"created_at"`
	Engine        string `json:"engine"`
	Catalog       string `json:"catalog,omitempty"`
	// SourceID 是备份来源的身份串（由调用方提供：sqlite 用绝对路径、网络引擎用
	// host:port/库名）。恢复时用它挡住「往源库上恢复」这种误操作。
	//
	// 它不来自驱动，所以只能防手滑，不能当安全边界——写在这里是为了让下一个人
	// 别把它当成认证手段（docs/08 §3.1 第 2 条只要求「同名库」这一层）。
	SourceID string         `json:"source_id,omitempty"`
	Objects  []BackupObject `json:"objects"`

	// Flags 是二进制头里的 flags 字段（不进 JSON）。读容器时由 InspectBackup
	// 填好；写入时由 Backup 按请求设置。
	Flags uint16 `json:"-"`
}

// BackupRequest 是一次备份请求。
type BackupRequest struct {
	// Tables 为空表示导出当前库的全部表与视图。
	Tables []string
	// Resume 表示接着上次未完成的备份继续（只看 BackupToFile）。
	Resume bool
	// SourceID 写进容器头，用于恢复时识别源库（见 BackupHeader.SourceID）。
	SourceID string
	// Compress 对每段做 gzip。文本型 SQL 通常能压到原来的 1/5～1/10。
	Compress bool

	// afterSection 是测试用的失败注入点：每写完一张表调用一次，返回错误即中断。
	// 用它模拟「中途被 kill」，比真的杀进程稳定，且能精确控制断在第几张表。
	afterSection func(table string) error
}

// Backup 把库（或指定表）写成一个 DBR 容器。
//
// 每张表的内容就是 Export 的 sql 格式脚本（建表 + INSERT），所以容器既可以被
// inspect 打开看清单，也可以逐段取出直接回放。
func (e *Executor) Backup(ctx context.Context, req BackupRequest, w io.Writer) (*BackupHeader, error) {
	tables, header, err := e.backupPlan(ctx, req)
	if err != nil {
		return nil, err
	}
	hj, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}

	if _, err := io.WriteString(w, backupMagic); err != nil {
		return nil, err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(backupFormatVersion)); err != nil {
		return nil, err
	}
	var flags uint16
	if req.Compress {
		flags |= backupFlagCompressed
	}
	if err := binary.Write(w, binary.LittleEndian, flags); err != nil {
		return nil, err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(hj))); err != nil {
		return nil, err
	}
	if _, err := w.Write(hj); err != nil {
		return nil, err
	}

	for _, name := range tables {
		var sec bytes.Buffer
		if _, err := e.Export(ctx, ExportRequest{Table: name, Format: ExportSQL}, &sec); err != nil {
			return nil, fmt.Errorf("导出 %s: %w", name, err)
		}
		if err := writeBackupSection(w, sec.Bytes(), req.Compress); err != nil {
			return nil, err
		}
	}
	return header, nil
}

// ErrBackupFormat 表示容器损坏或不是 DBR 文件。
var ErrBackupFormat = errors.New("不是有效的 DBR 备份文件")

// InspectBackup 只读头部，列出容器里有哪些对象。
//
// 这是「这个备份里到底有什么」的秒级回答：不解压、不连库。
func InspectBackup(r io.Reader) (*BackupHeader, error) {
	magic := make([]byte, len(backupMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackupFormat, err)
	}
	if string(magic) != backupMagic {
		return nil, fmt.Errorf("%w: 头部标识不匹配", ErrBackupFormat)
	}
	var ver, flags uint16
	var hlen uint32
	if err := binary.Read(r, binary.LittleEndian, &ver); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackupFormat, err)
	}
	if err := binary.Read(r, binary.LittleEndian, &flags); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackupFormat, err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hlen); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackupFormat, err)
	}
	if ver != backupFormatVersion {
		return nil, fmt.Errorf("%w: 格式版本 %d，本工具只认 %d", ErrBackupFormat, ver, backupFormatVersion)
	}
	if flags&backupFlagEncrypted != 0 {
		return nil, fmt.Errorf("%w: 这是加密容器，v1 不支持解密", ErrBackupFormat)
	}
	if flags & ^backupFlagCompressed != 0 {
		return nil, fmt.Errorf("%w: 未知的容器 flags（0x%02x）", ErrBackupFormat, flags)
	}
	if hlen == 0 || hlen > 16<<20 {
		return nil, fmt.Errorf("%w: header 长度异常（%d）", ErrBackupFormat, hlen)
	}
	raw := make([]byte, hlen)
	if _, err := io.ReadFull(r, raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBackupFormat, err)
	}
	var h BackupHeader
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("%w: header 解析失败: %v", ErrBackupFormat, err)
	}
	h.Flags = flags
	return &h, nil
}

// BackupSummary 把头部渲染成人看的清单（CLI 与界面共用）。
func BackupSummary(h *BackupHeader) string {
	var b strings.Builder
	packed := ""
	if h.Flags&backupFlagCompressed != 0 {
		// 压缩是容器属性，不在 header JSON 里；不说一声，用户会以为文件坏了
		// 或者怀疑体积对不上。
		packed = " · 已压缩"
	}
	fmt.Fprintf(&b, "DBR 备份 v%d · %s · %s%s · %d 个对象\n",
		h.FormatVersion, h.Engine, h.CreatedAt, packed, len(h.Objects))
	for _, o := range h.Objects {
		fmt.Fprintf(&b, "  %s %s\n", o.Kind, o.Name)
	}
	return b.String()
}

// ErrBackupNoCheckpoint 表示要求续跑但找不到检查点。
//
// 单独一个错误是为了让 CLI 能给出「需整库重跑」的明确提示——静默从头开始，
// 用户会以为自己续上了。
var ErrBackupNoCheckpoint = errors.New("找不到备份检查点")

// backupCheckpoint 是续跑检查点的一行。
//
// 粒度是「表」：每写完一张表记一条，续跑时截断到 End 再往下写。
// 侧车格式用 JSON Lines，坏一行能定位，不需要整体解析成功。
type backupCheckpoint struct {
	Table string `json:"table"`
	End   int64  `json:"end_offset"`
}

func backupProgressPath(path string) string { return path + ".progress" }

func readBackupProgress(path string) ([]backupCheckpoint, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []backupCheckpoint
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var cp backupCheckpoint
		if err := json.Unmarshal([]byte(line), &cp); err != nil {
			return nil, fmt.Errorf("检查点损坏（%s）：%w", path, err)
		}
		out = append(out, cp)
	}
	return out, nil
}

func appendBackupProgress(path string, cp backupCheckpoint) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// BackupToFile 把备份写到文件，支持按表续跑（ADR-0009）。
//
// 续跑口径：每写完一张表追加一条检查点到 <path>.progress；Resume 时读它、
// 把文件截断到最后一条记录的偏移、跳过已完成的表继续写。容器格式不变。
//
// 代价写在 ADR 里：单表失败要重导整表；侧车丢了只能整库重跑（会明确报错）。
func (e *Executor) BackupToFile(ctx context.Context, req BackupRequest, path string) (*BackupHeader, error) {
	tables, header, err := e.backupPlan(ctx, req)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	done := map[string]bool{}
	var pos int64
	if req.Resume {
		// 先看检查点，再看头部：没有检查点时文件可能压根是空的（新建），
		// 那时读头会得到「不是有效容器」，而用户真正需要知道的是「需整库重跑」。
		cps, err := readBackupProgress(backupProgressPath(path))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%w：%s（没有进度文件，需整库重跑）", ErrBackupNoCheckpoint, path)
			}
			return nil, err
		}
		for _, cp := range cps {
			done[cp.Table] = true
			pos = cp.End
		}
		if pos == 0 {
			return nil, fmt.Errorf("%w：%s（进度文件为空，需整库重跑）", ErrBackupNoCheckpoint, path)
		}
		// 有检查点说明文件里已经有头：用它来沿用压缩标志与源库身份，
		// 否则 -resume 时忘带 -compress 会把同一个文件写成两种格式。
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		existing, err := InspectBackup(f)
		if err != nil {
			return nil, fmt.Errorf("读取已写入的头部失败（需整库重跑）：%w", err)
		}
		header = existing

		if err := f.Truncate(pos); err != nil {
			return nil, fmt.Errorf("截断到最后一条检查点: %w", err)
		}
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			return nil, err
		}
	} else {
		if err := f.Truncate(0); err != nil {
			return nil, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		_ = os.Remove(backupProgressPath(path))

		hj, err := json.Marshal(header)
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(f, backupMagic); err != nil {
			return nil, err
		}
		if err := binary.Write(f, binary.LittleEndian, uint16(backupFormatVersion)); err != nil {
			return nil, err
		}
		var flags uint16
		if req.Compress {
			flags |= backupFlagCompressed
		}
		header.Flags = flags
		// 注意：flags 在 header 里是 json:"-"，改它不会影响已序列化的 hj；
		// 这里必须在 Marshal 之后才能写二进制头，二者是两份独立的事实。
		if err := binary.Write(f, binary.LittleEndian, flags); err != nil {
			return nil, err
		}
		if err := binary.Write(f, binary.LittleEndian, uint32(len(hj))); err != nil {
			return nil, err
		}
		if _, err := f.Write(hj); err != nil {
			return nil, err
		}
	}

	for _, name := range tables {
		if done[name] {
			continue
		}
		var sec bytes.Buffer
		if _, err := e.Export(ctx, ExportRequest{Table: name, Format: ExportSQL}, &sec); err != nil {
			return nil, fmt.Errorf("导出 %s: %w", name, err)
		}
		if err := writeBackupSection(f, sec.Bytes(), header.Flags&backupFlagCompressed != 0); err != nil {
			return nil, err
		}
		end, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		if err := appendBackupProgress(backupProgressPath(path), backupCheckpoint{Table: name, End: end}); err != nil {
			return nil, fmt.Errorf("写检查点: %w", err)
		}
		if req.afterSection != nil {
			if err := req.afterSection(name); err != nil {
				return nil, err
			}
		}
	}
	_ = os.Remove(backupProgressPath(path))
	return header, nil
}

// backupPlan 解析要导的表并构造头部（两条写入路径共用）。
func (e *Executor) backupPlan(ctx context.Context, req BackupRequest) ([]string, *BackupHeader, error) {
	tables := req.Tables
	if len(tables) == 0 {
		objs, err := e.conn.Meta().Objects(ctx, "", "", model.KindTable, model.KindView)
		if err != nil {
			return nil, nil, fmt.Errorf("列出对象: %w", err)
		}
		for _, o := range objs {
			tables = append(tables, o.Name)
		}
	}
	if len(tables) == 0 {
		return nil, nil, errors.New("备份：这个库里没有可导出的对象")
	}
	header := &BackupHeader{
		FormatVersion: backupFormatVersion,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		Engine:        string(e.conn.Engine()),
		SourceID:      req.SourceID,
	}
	for _, name := range tables {
		obj := BackupObject{Kind: string(model.KindTable), Name: name}
		// 数量取不到就不记（保持 0），恢复时自然跳过比对——不要为了凑数字让
		// 备份直接失败。
		if detail, err := e.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: name}); err == nil {
			obj.Indexes = len(detail.Indexes)
			obj.ForeignKeys = len(detail.ForeignKeys)
		}
		header.Objects = append(header.Objects, obj)
	}
	return tables, header, nil
}

// RestoreRequest 是一次恢复请求。
type RestoreRequest struct {
	// AllowExisting 允许覆盖目标库里已存在的同名对象。默认 false：
	// 「恢复到已有对象的库」必须由人显式同意（docs/08 §3.1 硬性约束）。
	AllowExisting bool
	// SourceID 是目标库的身份串。与容器头一致时拒绝——「禁止恢复到源库」
	// 是 docs/08 §3.1 的第 2 条硬性约束。
	SourceID string
}

// RestoreObject 是报告里的一个对象。
type RestoreObject struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
	// Indexes 是恢复后的索引个数（含主键约束背后的索引）。
	// 只报数量与存在性：索引的有效性（例如 PG 的 indisvalid）还没查。
	Indexes     int `json:"indexes"`
	ForeignKeys int `json:"foreign_keys"`
}

// RestoreReport 是恢复后的演练报告（docs/08 §3.3 的最小版本）。
type RestoreReport struct {
	Objects  []RestoreObject `json:"objects"`
	Rows     int64           `json:"rows"`
	Duration time.Duration   `json:"duration_ms"`
	// Mismatches 是「恢复后与备份时的计数对不上」的清单。空表示计数一致；
	// 非空不代表恢复失败（数据已经写进去了），但必须让人看见。
	Mismatches []string `json:"mismatches,omitempty"`
}

// ErrRestoreTarget 表示目标库不满足恢复前提。
var ErrRestoreTarget = errors.New("目标库不满足恢复前提")

// Restore 把容器恢复到当前连接。
//
// v1 的三条硬约束（都可以在错误里读到原因，不静默降级）：
//  1. **同引擎**：跨引擎要类型映射与冲突判定，v1 不做；
//  2. **目标非空默认拒绝**：容器里任一对象名已在目标库存在，就整批不执行，
//     除非显式 AllowExisting；
//  3. **单事务**：引擎支持 DDL 事务时全程一个事务，任何一段失败整体回滚。
//
// 已知缺口：容器头还没有记录源库身份，所以「禁止恢复到源库同名库」这条目前
// 只能靠调用方自律（见 docs/08 §7 的 v1 口径）。
func (e *Executor) Restore(ctx context.Context, r io.Reader, req RestoreRequest) (*RestoreReport, error) {
	started := time.Now()
	header, sections, err := readBackupContainer(r)
	if err != nil {
		return nil, err
	}
	if header.SourceID != "" && req.SourceID != "" && header.SourceID == req.SourceID {
		return nil, fmt.Errorf("%w：这是该备份的源库（%s），禁止恢复到源库",
			ErrRestoreTarget, header.SourceID)
	}
	if header.Engine != string(e.conn.Engine()) {
		return nil, fmt.Errorf("%w：容器来自 %s，当前连接是 %s（v1 只支持同引擎恢复）",
			ErrRestoreTarget, header.Engine, e.conn.Engine())
	}

	// 目标非空检查：先看容器要建的对象在目标库里是否已经存在。
	existing, err := e.conn.Meta().Objects(ctx, "", "", model.KindTable, model.KindView)
	if err != nil {
		return nil, fmt.Errorf("列出目标对象: %w", err)
	}
	present := map[string]bool{}
	for _, o := range existing {
		present[o.Name] = true
	}
	var clash []string
	for _, o := range header.Objects {
		if present[o.Name] {
			clash = append(clash, o.Name)
		}
	}
	if len(clash) > 0 && !req.AllowExisting {
		return nil, fmt.Errorf("%w：目标库里已有同名对象 %s；确认要覆盖请显式允许",
			ErrRestoreTarget, strings.Join(clash, "、"))
	}

	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()

	inTx := e.conn.Capabilities().DDLTransactional
	if inTx {
		if err := sess.Begin(ctx); err != nil {
			return nil, err
		}
	}
	rollback := func() {
		if inTx {
			_ = sess.Rollback(context.Background())
		}
	}

	for i, sec := range sections {
		name := ""
		if i < len(header.Objects) {
			name = header.Objects[i].Name
		}
		stmts, err := e.dialect.Split(sec)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("切分 %s 的脚本: %w", name, err)
		}
		for _, st := range stmts {
			text := stripSQLComments(st.Text)
			if text == "" {
				continue
			}
			if _, err := sess.Execute(ctx, text, nil); err != nil {
				rollback()
				return nil, fmt.Errorf("恢复 %s 失败：%w", name, err)
			}
		}
	}
	if inTx {
		if err := sess.Commit(ctx); err != nil {
			return nil, err
		}
	}

	rep := &RestoreReport{}
	for _, o := range header.Objects {
		var n int64
		cur, err := sess.Query(ctx, "SELECT COUNT(*) FROM "+e.dialect.QuoteIdent(o.Name), nil,
			driver.QueryOptions{MaxRows: 1})
		if err == nil {
			if rows, _, err2 := cur.Next(ctx); err2 == nil && len(rows) == 1 {
				n = countValue(rows[0][0])
			}
			_ = cur.Close()
		}
		idx, fks := 0, 0
		if detail, derr := e.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: o.Name}); derr == nil {
			idx = len(detail.Indexes)
			fks = len(detail.ForeignKeys)
		}
		rep.Objects = append(rep.Objects, RestoreObject{Name: o.Name, Rows: n, Indexes: idx, ForeignKeys: fks})
		rep.Rows += n
		// 与备份时的计数比对。老容器没有这两个字段（0）时跳过——不能把
		// 「当时没记」报成「恢复丢了」。
		if o.Indexes > 0 && idx != o.Indexes {
			rep.Mismatches = append(rep.Mismatches,
				fmt.Sprintf("%s：索引 %d 个，备份时 %d 个", o.Name, idx, o.Indexes))
		}
		if o.ForeignKeys > 0 && fks != o.ForeignKeys {
			rep.Mismatches = append(rep.Mismatches,
				fmt.Sprintf("%s：外键 %d 个，备份时 %d 个", o.Name, fks, o.ForeignKeys))
		}
	}
	rep.Duration = time.Since(started)
	return rep, nil
}

// readBackupContainer 读完整个容器（头 + 每段）。
//
// v1 一次性读进内存：容器里每段本来就是该表的完整脚本，恢复时也按段执行，
// 拆成流式读取的收益要到「超大库」才显现，先不为它增加状态机。
func readBackupContainer(r io.Reader) (*BackupHeader, []string, error) {
	h, err := InspectBackup(r)
	if err != nil {
		return nil, nil, err
	}
	compressed := h.Flags&backupFlagCompressed != 0
	var sections []string
	for {
		sec, err := readBackupSection(r, compressed)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, nil, err
		}
		sections = append(sections, sec)
	}
	return h, sections, nil
}

// stripSQLComments 去掉行注释（脚本头部的说明），空行也一并丢掉。
func stripSQLComments(text string) string {
	var keep []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

func countValue(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case []byte:
		var n int64
		fmt.Sscanf(string(x), "%d", &n)
		return n
	case string:
		var n int64
		fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}

// writeBackupSection 写一段：u32 长度 + （可选 gzip）内容。
//
// 压缩开关记在头的 flags 里，读端据此解压；段本身不带标志，避免同一份文件里
// 两种形态混用。
func writeBackupSection(w io.Writer, data []byte, compress bool) error {
	if compress {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(data); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		data = buf.Bytes()
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// readBackupSection 读一段，按需解压。
func readBackupSection(r io.Reader, compressed bool) (string, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n > 1<<30 {
		return "", fmt.Errorf("%w: 段长度异常（%d）", ErrBackupFormat, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("%w: 段内容不完整", ErrBackupFormat)
	}
	if !compressed {
		return string(buf), nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("%w: 解压失败: %v", ErrBackupFormat, err)
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(zr)
	if err != nil {
		return "", fmt.Errorf("%w: 解压失败: %v", ErrBackupFormat, err)
	}
	return string(out), nil
}
