package service

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// migrateCheckpoint 是数据搬运的检查点（ADR-0011：主键键集 + 累计摘要）。
//
// 只落「下一批从哪开始」与「累计到哪」，不落已经搬过的数据——续跑靠键集跳过，
// 不靠重放。
type migrateCheckpoint struct {
	Table  string            `json:"table"`
	Rows   int64             `json:"rows"`
	Last   []checkpointValue `json:"last"`
	Digest string            `json:"digest,omitempty"`
}

// checkpointValue 是带类型标记的键值。
//
// 不做类型标记的话，"123" 与 123 续跑时会喂错类型的参数——MySQL 可能隐式转换，
// PG 直接报类型不匹配。
type checkpointValue struct {
	Kind string `json:"k"`
	Int  int64  `json:"i,omitempty"`
	Str  string `json:"s,omitempty"`
	Time string `json:"t,omitempty"`
}

func encodeCheckpointValue(v any) (checkpointValue, error) {
	switch x := v.(type) {
	case int64:
		return checkpointValue{Kind: "i", Int: x}, nil
	case int:
		return checkpointValue{Kind: "i", Int: int64(x)}, nil
	case string:
		return checkpointValue{Kind: "s", Str: x}, nil
	case []byte:
		return checkpointValue{Kind: "s", Str: string(x)}, nil
	case time.Time:
		return checkpointValue{Kind: "t", Time: x.UTC().Format(time.RFC3339Nano)}, nil
	default:
		return checkpointValue{}, fmt.Errorf("迁移续跑暂不支持这种主键类型（%T）；本表可一次性搬完", v)
	}
}

func decodeCheckpointValue(c checkpointValue) (any, error) {
	switch c.Kind {
	case "i":
		return c.Int, nil
	case "s":
		return c.Str, nil
	case "t":
		tm, err := time.Parse(time.RFC3339Nano, c.Time)
		if err != nil {
			return nil, fmt.Errorf("检查点里的时间不合法: %w", err)
		}
		return tm, nil
	default:
		return nil, fmt.Errorf("检查点里的类型标记未知: %q", c.Kind)
	}
}

func saveMigrateCheckpoint(path string, cp migrateCheckpoint) error {
	b, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	// 写临时文件再改名：进程被杀时不会留下半截 JSON。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadMigrateCheckpoint(path, table string) (*migrateCheckpoint, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cp migrateCheckpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, fmt.Errorf("检查点损坏（%s）：%w", path, err)
	}
	if cp.Table != table {
		return nil, fmt.Errorf("检查点属于表 %q，与本次要搬的 %q 不一致", cp.Table, table)
	}
	return &cp, nil
}

// digestFromHex 恢复累计摘要状态。
func digestFromHex(s string, rows int64) (*tableDigest, error) {
	if s == "" {
		return &tableDigest{}, nil
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("检查点里的摘要不合法")
	}
	d := &tableDigest{}
	copy(d.h[:], b)
	d.n = rows
	d.init = true
	return d, nil
}
