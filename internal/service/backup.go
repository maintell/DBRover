package service

import (
	"bytes"
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
)

// BackupObject 是容器头部里的一个对象条目。
//
// v1 不含行数与大小：它们要等表导完才知道，而头必须在最前面。
// 需要行数时请从段里的脚本自行统计，或用「导出统计」接口。
type BackupObject struct {
	Kind   string `json:"kind"`
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
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
}

// BackupRequest 是一次备份请求。
type BackupRequest struct {
	// Tables 为空表示导出当前库的全部表与视图。
	Tables []string
	// Resume 表示接着上次未完成的备份继续（只看 BackupToFile）。
	Resume bool
	// SourceID 写进容器头，用于恢复时识别源库（见 BackupHeader.SourceID）。
	SourceID string

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
	if err := binary.Write(w, binary.LittleEndian, uint16(0)); err != nil {
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
		if err := binary.Write(w, binary.LittleEndian, uint32(sec.Len())); err != nil {
			return nil, err
		}
		if _, err := w.Write(sec.Bytes()); err != nil {
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
	if flags != 0 {
		return nil, fmt.Errorf("%w: v1 不支持加密/压缩的容器", ErrBackupFormat)
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
	return &h, nil
}

// BackupSummary 把头部渲染成人看的清单（CLI 与界面共用）。
func BackupSummary(h *BackupHeader) string {
	var b strings.Builder
	fmt.Fprintf(&b, "DBR 备份 v%d · %s · %s · %d 个对象\n",
		h.FormatVersion, h.Engine, h.CreatedAt, len(h.Objects))
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
		if err := binary.Write(f, binary.LittleEndian, uint16(0)); err != nil {
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
		if err := binary.Write(f, binary.LittleEndian, uint32(sec.Len())); err != nil {
			return nil, err
		}
		if _, err := f.Write(sec.Bytes()); err != nil {
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
		header.Objects = append(header.Objects, BackupObject{Kind: string(model.KindTable), Name: name})
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
	Indexes int `json:"indexes"`
}

// RestoreReport 是恢复后的演练报告（docs/08 §3.3 的最小版本）。
type RestoreReport struct {
	Objects  []RestoreObject `json:"objects"`
	Rows     int64           `json:"rows"`
	Duration time.Duration   `json:"duration_ms"`
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
		idx := 0
		if detail, derr := e.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: o.Name}); derr == nil {
			idx = len(detail.Indexes)
		}
		rep.Objects = append(rep.Objects, RestoreObject{Name: o.Name, Rows: n, Indexes: idx})
		rep.Rows += n
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
	var sections []string
	for {
		var n uint32
		if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, nil, fmt.Errorf("%w: 读段长度: %v", ErrBackupFormat, err)
		}
		if n > 1<<30 {
			return nil, nil, fmt.Errorf("%w: 段长度异常（%d）", ErrBackupFormat, n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, nil, fmt.Errorf("%w: 段内容不完整", ErrBackupFormat)
		}
		sections = append(sections, string(buf))
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
