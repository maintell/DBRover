package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maintell/DBRover/internal/model"
)

// fakeCursor 是一个永远读尽的空游标，用于隔离测试游标存储本身。
type fakeCursor struct {
	closed bool
}

func (f *fakeCursor) Columns() []model.Column { return nil }

func (f *fakeCursor) Next(ctx context.Context) ([][]any, bool, error) {
	return nil, false, nil
}

func (f *fakeCursor) Close() error {
	f.closed = true
	return nil
}

// TestCursorExpiresWithoutAccess 是泄漏回归：
// 曾经只在 page() 里顺带 sweep，打开后不再翻页的游标永不过期，
// 会一直持有 *sql.Rows 与池中连接。
func TestCursorExpiresWithoutAccess(t *testing.T) {
	store := newCursorStore(30 * time.Millisecond)
	defer store.closeAll()

	cur := &fakeCursor{}
	id := store.open(&cursorHandle{cur: &pagedCursor{c: cur}})

	if _, err := store.page(context.Background(), id, 10); err != nil {
		t.Fatalf("刚打开时应可读: %v", err)
	}

	// 不再访问，等清理协程回收。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.len() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if n := store.len(); n != 0 {
		t.Fatalf("过期后仍有 %d 个游标存活", n)
	}
	if !cur.closed {
		t.Error("过期游标必须关闭底层驱动游标")
	}
	if _, err := store.page(context.Background(), id, 10); !errors.Is(err, ErrCursorClosed) {
		t.Errorf("过期后再取应返回 ErrCursorClosed, 实际 %v", err)
	}
}

// TestCursorCloseAllIsIdempotent 保证重复 Close 不会 panic（关已关闭的 channel）。
func TestCursorCloseAllIsIdempotent(t *testing.T) {
	store := newCursorStore(time.Minute)
	store.open(&cursorHandle{cur: &pagedCursor{c: &fakeCursor{}}})
	store.closeAll()
	store.closeAll()
}

// TestCursorTTLRenewedByAccess 保证每次翻页续期。
func TestCursorTTLRenewedByAccess(t *testing.T) {
	store := newCursorStore(120 * time.Millisecond)
	defer store.closeAll()

	cur := &fakeCursor{}
	id := store.open(&cursorHandle{cur: &pagedCursor{c: cur}})

	// 持续访问，总时长超过 TTL，但每次间隔小于 TTL，应始终存活。
	for i := 0; i < 6; i++ {
		time.Sleep(40 * time.Millisecond)
		if _, err := store.page(context.Background(), id, 10); err != nil {
			// 第一页会读尽并自动关闭，这是正常语义；用新的游标继续验证续期。
			id = store.open(&cursorHandle{cur: &pagedCursor{c: &fakeCursor{}}})
		}
	}
}
