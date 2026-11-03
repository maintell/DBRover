package service

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// ErrAnchorMAC 表示锚点文件里的记录与 MAC 对不上。
var ErrAnchorMAC = errors.New("audit: 锚点记录校验失败（锚点文件被改过，或密钥不对）")

// AnchorRecord 是锚点文件里的一行。
//
// 它记的是「某一刻，审计链有多少条、链头哈希是什么」，并带一个用本地密钥
// 算出的 MAC。**MAC 是这条设计的关键**：链式哈希自己算得出来，攻击者整段
// 重写时也能算出一条自洽的新链；但重算 MAC 需要密钥，而密钥在 OS 凭据库里。
type AnchorRecord struct {
	At     time.Time `json:"at"`
	Events int       `json:"events"`
	Head   string    `json:"head"`
	MAC    string    `json:"mac"`
}

// Anchor 把审计链的链头定期写到一个独立文件里。
//
// 为什么单独一个文件而不是写进审计文件本身：写在一起的话，重写者可以连锚
// 一起重写。分开之后，要伪造必须同时改两个文件**并且**拿到密钥。
type Anchor struct {
	mu   sync.Mutex
	path string
	key  []byte
}

// OpenAnchor 打开（或创建）锚点文件。
//
// key 是 MAC 用的对称密钥，调用方负责从系统凭据库取；为空直接报错——
// 没有密钥的锚点等于一张自签的收据，挡不住任何人。
func OpenAnchor(path string, key []byte) (*Anchor, error) {
	if path == "" {
		return nil, errors.New("audit: 锚点路径为空")
	}
	if len(key) == 0 {
		return nil, errors.New("audit: 锚点缺少密钥")
	}
	if dir := filepathDir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("创建锚点目录失败: %w", err)
		}
	}
	return &Anchor{path: path, key: key}, nil
}

// filepathDir 只用来避免在 anchor.go 里再引一次 path/filepath 造成重复导入困惑。
func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return ""
}

// mac 算一条记录的 MAC：HMAC-SHA256(key, events|head)。
func (a *Anchor) mac(events int, head string) string {
	h := hmac.New(sha256.New, a.key)
	fmt.Fprintf(h, "%d|%s", events, head)
	return hex.EncodeToString(h.Sum(nil))
}

// Seal 追加一条锚点记录。events 是当前链上的事件数，head 是链头哈希。
func (a *Anchor) Seal(events int, head string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	rec := AnchorRecord{At: time.Now(), Events: events, Head: head, MAC: a.mac(events, head)}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开锚点文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("写锚点失败: %w", err)
	}
	return nil
}

// Latest 读最后一条锚点记录并验 MAC。文件为空时返回 (nil, nil)。
func (a *Anchor) Latest() (*AnchorRecord, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	f, err := os.Open(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var last *AnchorRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var rec AnchorRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("锚点文件第 %d 行不是合法 JSON: %w", len(raw), err)
		}
		if !hmac.Equal([]byte(rec.MAC), []byte(a.mac(rec.Events, rec.Head))) {
			return nil, ErrAnchorMAC
		}
		r := rec
		last = &r
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return last, nil
}
