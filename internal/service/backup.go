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
	// Type 是这个容器装了什么：`full`（结构 + 数据）或 `schema`（只有结构）。
	//
	// 记在头里而不是只记在请求里：恢复与 inspect 都要知道「这个文件里有没有数据」——
	// 一个只有结构的备份拿去恢复，得到的是空表，而用户在 inspect 时就应该看见这一点。
	// 老容器没有这个字段（omitempty），按 full 处理。
	Type string `json:"type,omitempty"`
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

	// 加密参数（只有加密容器才有值）。盐与迭代次数写明文头，因为派生前必须
	// 先拿到它们；这不是秘密，真正的秘密只有口令。
	KDF        string `json:"kdf,omitempty"`
	Cipher     string `json:"cipher,omitempty"`
	Salt       string `json:"salt,omitempty"`
	Iterations int    `json:"iterations,omitempty"`
}

// BackupRequest 是一次备份请求。
type BackupRequest struct {
	// Tables 为空表示导出当前库的全部表与视图。
	Tables []string
	// Resume 表示接着上次未完成的备份继续（只看 BackupToFile）。
	Resume bool
	// SourceID 写进容器头，用于恢复时识别源库（见 BackupHeader.SourceID）。
	SourceID string
	// Type 决定每段写什么：空或 `full` 写「建表 + INSERT」，`schema` 只写建表与索引。
	// 其它取值一律拒绝（含 `data`：只灌数据要求目标端结构已存在且完全一致，v1 不做）。
	Type string
	// Compress 对每段做 gzip。文本型 SQL 通常能压到原来的 1/5～1/10。
	Compress bool
	// Passphrase 非空时加密每段（AES-256-GCM，密钥由 PBKDF2 派生）。
	// 口令由调用方从 stdin 读，不作为命令行参数（进程列表可见）。
	Passphrase string

	// afterSection 是测试用的失败注入点：每写完一张表调用一次，返回错误即中断。
	// 用它模拟「中途被 kill」，比真的杀进程稳定，且能精确控制断在第几张表。
	afterSection func(table string) error
}

// 备份类型。
const (
	// BackupTypeFull 是结构 + 数据（默认）。
	BackupTypeFull = "full"
	// BackupTypeSchema 只有结构：建表 + 索引，不读任何行。
	//
	// 用途是「建空库」「结构比对」「把结构带走给人看」——这些场景拿一个带全量数据的
	// 文件既慢又危险（多一份业务数据落在磁盘上）。
	BackupTypeSchema = "schema"
	// BackupTypeData 只用于给出一句明确的拒绝理由，不做实现。
	BackupTypeData = "data"
)

// normalizeBackupType 把请求里的类型收敛成 full / schema。
//
// 为什么对 `data` 单独给一句理由：只说「不支持」的话，用户会以为是参数写法不对，
// 反复试；说清「要目标端结构已存在且完全一致」才知道该怎么做。
func normalizeBackupType(t string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", BackupTypeFull:
		return BackupTypeFull, nil
	case BackupTypeSchema:
		return BackupTypeSchema, nil
	case BackupTypeData:
		return "", errors.New("备份类型 data（仅数据）未实现：它要求目标端结构已存在且完全一致，v1 还没有这个能力；" +
			"要灌数据请先恢复结构再用 /exec 或 dbrover exec 手工导")
	default:
		return "", fmt.Errorf("不认识的备份类型 %q：只支持 full（默认）与 schema", t)
	}
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
	key, err := prepareBackupKey(req.Passphrase, header)
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
	if key != nil {
		flags |= backupFlagEncrypted
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
		if _, err := e.Export(ctx, ExportRequest{
			Table: name, Format: ExportSQL,
			SchemaOnly: header.Type == BackupTypeSchema,
		}, &sec); err != nil {
			return nil, fmt.Errorf("导出 %s: %w", name, err)
		}
		if err := writeBackupSection(w, sec.Bytes(), req.Compress, key); err != nil {
			return nil, err
		}
	}
	return header, nil
}

// prepareBackupKey 在需要加密时生成盐、填好头部参数并派生密钥。
//
// 头里只放盐与迭代次数（派生要用），密钥本身不落盘。
func prepareBackupKey(passphrase string, header *BackupHeader) ([]byte, error) {
	if passphrase == "" {
		return nil, nil
	}
	salt, saltB64, err := newBackupSalt()
	if err != nil {
		return nil, err
	}
	header.KDF = backupKDF
	header.Cipher = backupCipher
	header.Salt = saltB64
	header.Iterations = backupKDFIterations
	return deriveBackupKey(passphrase, salt, backupKDFIterations)
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
	// 加密容器的头仍然是明文（要能列出对象、要能拿到盐），所以这里不拦；
	// 解密在 Restore 里按口令做。未知位必须拒绝——静默忽略会让读端解出错误数据。
	if flags & ^(backupFlagCompressed|backupFlagEncrypted) != 0 {
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
	// 压缩/加密是容器属性（flags），不在 header JSON 里；不说一声，用户会以为
	// 文件坏了、或者怀疑某个备份没加密。
	packed := ""
	if h.Flags&backupFlagCompressed != 0 {
		packed += " · 已压缩"
	}
	if h.Flags&backupFlagEncrypted != 0 {
		packed += " · 已加密"
	}
	// 类型必须露出来：一个只有结构的备份拿去恢复会得到空表，用户在 inspect 这一步
	// 就该看见「这里面没有数据」，而不是恢复之后才发现。老容器没有 Type 字段，按 full 说。
	kind := h.Type
	if kind == "" {
		kind = BackupTypeFull
	}
	label := "结构 + 数据"
	if kind == BackupTypeSchema {
		label = "仅结构（无数据）"
	}
	fmt.Fprintf(&b, "DBR 备份 v%d · %s · %s · %s%s · %d 个对象\n",
		h.FormatVersion, h.Engine, label, h.CreatedAt, packed, len(h.Objects))
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
		// 续跑沿用文件里的类型：段格式必须与已写入的段一致，中途改类型会把
		// 「建表 + INSERT」与「只有建表」混进同一个文件。
		if t, terr := normalizeBackupType(req.Type); terr != nil {
			return nil, terr
		} else if header.Type != "" && t != header.Type {
			return nil, fmt.Errorf("续跑不能改备份类型：文件里是 %s，本次要求 %s", header.Type, t)
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

		freshKey, err := prepareBackupKey(req.Passphrase, header)
		if err != nil {
			return nil, err
		}
		_ = freshKey // 密钥在后面按头部的盐统一派生
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
		if header.KDF != "" {
			flags |= backupFlagEncrypted
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

	// 写入端的密钥：加密由头部参数决定。续跑时头部来自文件，因此这里必须
	// 按文件里的盐重新派生，而不是按本次请求——否则新旧段用两把不同的钥匙。
	writeKey, err := writeKeyForHeader(header, req.Passphrase)
	if err != nil {
		return nil, err
	}

	for _, name := range tables {
		if done[name] {
			continue
		}
		var sec bytes.Buffer
		if _, err := e.Export(ctx, ExportRequest{
			Table: name, Format: ExportSQL,
			SchemaOnly: header.Type == BackupTypeSchema,
		}, &sec); err != nil {
			return nil, fmt.Errorf("导出 %s: %w", name, err)
		}
		if err := writeBackupSection(f, sec.Bytes(), header.Flags&backupFlagCompressed != 0, writeKey); err != nil {
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
	kind, err := normalizeBackupType(req.Type)
	if err != nil {
		return nil, nil, err
	}
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
		Type:          kind,
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
	// ConfirmTarget 是要覆盖时必填的「目标标识」：必须与连接器自报的
	// Target() 一致（关系库是库名，SQLite 是文件路径）。
	//
	// 为什么不用布尔开关：布尔开关挡得住手滑，挡不住「看错目标库」——它不需要
	// 人回忆任何东西。要求把目标名写出来，选错库的人会发现自己写的与实际指向
	// 的不是同一个（见 ADR-0015）。
	ConfirmTarget string
	// SourceID 是目标库的身份串。与容器头一致时拒绝——「禁止恢复到源库」
	// 是 docs/08 §3.1 的第 2 条硬性约束。
	SourceID string
	// Passphrase 是加密容器的口令；容器加密却没给会返回
	// ErrBackupNeedPassphrase，口令不对返回 ErrBackupBadPassphrase。
	Passphrase string
}

// restoreTargetLabel 给错误信息用的目标名；连接器报不出名字时说明白，
// 而不是留一个空白引号让人猜。
func restoreTargetLabel(target string) string {
	if target == "" {
		return "（这条连接没有报告目标标识）"
	}
	return target
}

// RestoreObject 是报告里的一个对象。
type RestoreObject struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
	// Indexes 是恢复后的索引个数（含主键约束背后的索引）。
	Indexes     int `json:"indexes"`
	ForeignKeys int `json:"foreign_keys"`
	// InvalidIndexes 是「建出来了但不可用」的索引名（PG 的 indisvalid=false）。
	// 有它就必须出声：索引在清单里、却完全不参与查询，唯一索引也不再保证唯一。
	InvalidIndexes []string `json:"invalid_indexes,omitempty"`
}

// RestoreReport 是恢复后的演练报告（docs/08 §3.3 的最小版本）。
type RestoreReport struct {
	Objects []RestoreObject `json:"objects"`
	// Replaced 是覆盖模式下被先删掉、再按备份重建的对象名。非空意味着
	// 目标库里这些对象原来的数据已经没了——报告必须把它说出来。
	Replaced []string      `json:"replaced,omitempty"`
	Rows     int64         `json:"rows"`
	Duration time.Duration `json:"duration_ms"`
	// Mismatches 是「恢复后与备份时的计数对不上」的清单。空表示计数一致；
	// 非空不代表恢复失败（数据已经写进去了），但必须让人看见。
	Mismatches []string `json:"mismatches,omitempty"`
	// Warnings 是「数量对得上、但东西不可用」的提醒（例如无效索引）。
	Warnings []string `json:"warnings,omitempty"`
}

// ErrRestoreTarget 表示目标库不满足恢复前提。
var ErrRestoreTarget = errors.New("目标库不满足恢复前提")

// Restore 把容器恢复到当前连接。
//
// v1 的三条硬约束（都可以在错误里读到原因，不静默降级）：
//  1. **同引擎**：跨引擎要类型映射与冲突判定，v1 不做；
//  2. **目标非空默认拒绝**：容器里任一对象名已在目标库存在，就整批不执行，
//     除非调用方把目标名写进 ConfirmTarget 且与连接器自报的 Target() 一致；
//  3. **单事务**：引擎支持 DDL 事务时全程一个事务，任何一段失败整体回滚。
//
// 已知缺口：容器头还没有记录源库身份，所以「禁止恢复到源库同名库」这条目前
// 只能靠调用方自律（见 docs/08 §7 的 v1 口径）。
func (e *Executor) Restore(ctx context.Context, r io.Reader, req RestoreRequest) (*RestoreReport, error) {
	started := time.Now()
	header, sections, err := readBackupContainer(r, req.Passphrase)
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
	kindOf := map[string]model.ObjectKind{}
	for _, o := range existing {
		kindOf[o.Name] = o.Kind
	}
	schemaByName := map[string]string{}
	for _, o := range existing {
		schemaByName[o.Name] = o.Schema
	}
	var clash []string
	for _, o := range header.Objects {
		if present[o.Name] {
			clash = append(clash, o.Name)
		}
	}
	if len(clash) > 0 {
		target := strings.TrimSpace(e.conn.Target())
		confirmed := strings.TrimSpace(req.ConfirmTarget)
		switch {
		case confirmed == "":
			return nil, fmt.Errorf("%w：目标 %s 已有同名对象 %s；确认要覆盖，请把目标名原样写出来（ConfirmTarget）",
				ErrRestoreTarget, restoreTargetLabel(target), strings.Join(clash, "、"))
		case !strings.EqualFold(confirmed, target):
			// 不用 %q：它会转义 Windows 路径里的反斜杠，人读起来像乱码。
			return nil, fmt.Errorf("%w：你写的目标名是「%s」，而这条连接实际指向「%s」 —— 先确认有没有连错库",
				ErrRestoreTarget, confirmed, restoreTargetLabel(target))
		}
	}

	sess, err := e.conn.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()

	inTx := e.conn.Capabilities().DDLTransactional
	// 覆盖模式要先把冲突对象删掉，而「删」是不可逆的破坏：只有在 DDL 能整体
	// 回滚的引擎上才做。MySQL 的 DDL 隐式提交，中途失败会留下「旧的被删了、
	// 新的没建起来」的库——那种事不该由一个开关授权。
	if len(clash) > 0 && !inTx {
		return nil, fmt.Errorf("%w：目标 %s 已有同名对象，而 %s 的 DDL 不能回滚，"+
			"覆盖会变成不可撤销的破坏；v1 不支持，请先清空目标库",
			ErrRestoreTarget, restoreTargetLabel(strings.TrimSpace(e.conn.Target())), e.conn.Engine())
	}
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

	// 覆盖：先删视图（它们依赖表），再删表。都在同一个事务里，失败整体回滚。
	var replaced []string
	if len(clash) > 0 {
		for _, pass := range []model.ObjectKind{model.KindView, model.KindTable} {
			for _, name := range clash {
				if kindOf[name] != pass {
					continue
				}
				quoted := e.dialect.QuoteQualified(schemaByName[name], name)
				text := "DROP TABLE " + quoted
				if pass == model.KindView {
					text = e.dialect.DropViewSQL(quoted)
				}
				if _, err := sess.Execute(ctx, text, nil); err != nil {
					rollback()
					return nil, fmt.Errorf("覆盖前删除 %s 失败：%w", name, err)
				}
				replaced = append(replaced, name)
			}
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

	rep := &RestoreReport{Replaced: replaced}
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
		var invalid []string
		if detail, derr := e.DescribeTable(ctx, model.ObjectRef{Kind: model.KindTable, Name: o.Name}); derr == nil {
			idx = len(detail.Indexes)
			fks = len(detail.ForeignKeys)
			invalid = InvalidIndexNames(detail)
		}
		rep.Objects = append(rep.Objects, RestoreObject{
			Name: o.Name, Rows: n, Indexes: idx, ForeignKeys: fks, InvalidIndexes: invalid,
		})
		for _, name := range invalid {
			rep.Warnings = append(rep.Warnings,
				fmt.Sprintf("%s：索引 %s 不可用（indisvalid=false），应重建", o.Name, name))
		}
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
func readBackupContainer(r io.Reader, passphrase string) (*BackupHeader, []string, error) {
	h, err := InspectBackup(r)
	if err != nil {
		return nil, nil, err
	}
	compressed := h.Flags&backupFlagCompressed != 0
	key, err := restoreKeyFromHeader(h, passphrase)
	if err != nil {
		return nil, nil, err
	}
	var sections []string
	for {
		sec, err := readBackupSection(r, compressed, key)
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
func writeBackupSection(w io.Writer, data []byte, compress bool, key []byte) error {
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
	if key != nil {
		enc, err := encryptSection(key, data)
		if err != nil {
			return err
		}
		data = enc
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// readBackupSection 读一段，按需解压。
func readBackupSection(r io.Reader, compressed bool, key []byte) (string, error) {
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
	if key != nil {
		plain, err := decryptSection(key, buf)
		if err != nil {
			return "", err
		}
		buf = plain
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

// InvalidIndexNames 列出「建出来了但不可用」的索引名。
//
// 为什么单列一个函数：这是「数量对得上、东西不可用」这一类问题的唯一入口，
// 恢复报告与将来的体检都要用它；散在调用点上，早晚会出现一处查 Valid、另一处
// 只看 len(Indexes) 的情况，而后者会把坏索引报成好索引。
//
// 只有 Valid 非 nil 且为 false 才算不可用：nil 表示引擎没有这个概念
// （MySQL / SQLite），不是「无效」。
func InvalidIndexNames(detail *model.TableDetail) []string {
	if detail == nil {
		return nil
	}
	var out []string
	for _, ix := range detail.Indexes {
		if ix.Valid != nil && !*ix.Valid {
			out = append(out, ix.Name)
		}
	}
	return out
}
