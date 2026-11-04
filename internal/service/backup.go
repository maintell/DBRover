package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

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
	FormatVersion int            `json:"format_version"`
	CreatedAt     string         `json:"created_at"`
	Engine        string         `json:"engine"`
	Catalog       string         `json:"catalog,omitempty"`
	Objects       []BackupObject `json:"objects"`
}

// BackupRequest 是一次备份请求。
type BackupRequest struct {
	// Tables 为空表示导出当前库的全部表与视图。
	Tables []string
}

// Backup 把库（或指定表）写成一个 DBR 容器。
//
// 每张表的内容就是 Export 的 sql 格式脚本（建表 + INSERT），所以容器既可以被
// inspect 打开看清单，也可以逐段取出直接回放。
func (e *Executor) Backup(ctx context.Context, req BackupRequest, w io.Writer) (*BackupHeader, error) {
	started := time.Now()
	tables := req.Tables
	if len(tables) == 0 {
		objs, err := e.conn.Meta().Objects(ctx, "", "", model.KindTable, model.KindView)
		if err != nil {
			return nil, fmt.Errorf("列出对象: %w", err)
		}
		for _, o := range objs {
			tables = append(tables, o.Name)
		}
	}
	if len(tables) == 0 {
		return nil, errors.New("备份：这个库里没有可导出的对象")
	}

	header := &BackupHeader{
		FormatVersion: backupFormatVersion,
		CreatedAt:     started.UTC().Format(time.RFC3339),
		Engine:        string(e.conn.Engine()),
		Catalog:       "",
	}
	for _, name := range tables {
		header.Objects = append(header.Objects, BackupObject{Kind: string(model.KindTable), Name: name})
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
