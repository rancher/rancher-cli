package config

import "sync"

// NoticeSource structurally matches runtime.NoticeSource without importing
// it (config sits below runtime in the dependency graph): a source of
// buffered operator-facing notices, drained destructively in record order.
// Used internally to type-assert a lazily-resolved backend so lazyStore can
// drain its own notices too, not just the pin notice recorded before
// resolution.
type NoticeSource interface {
	DrainNotices() []string
}

// noticeBuffer is a small, mutex-guarded queue of operator-facing notices
// recorded outside any pipeline Result — a credential store's first-pin
// notice, a keychain-degrade warning. Safe against a command-body goroutine
// racing a lazily-resolving store: record and DrainNotices may be called
// concurrently. A nil *noticeBuffer is a valid, permanently-empty source
// (record is then a no-op and DrainNotices returns nil), so a store
// constructed directly in a test without going through its normal
// constructor never panics merely for lacking one.
type noticeBuffer struct {
	mu      sync.Mutex
	notices []string
}

// record appends msg to the buffer in call order. A blank msg is a no-op —
// callers may pass through a "no notice" zero value (warnLoosePerm's ""
// return) without checking it first.
func (b *noticeBuffer) record(msg string) {
	if b == nil || msg == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.notices = append(b.notices, msg)
}

// DrainNotices empties the buffer and returns what it held, in record
// order. Destructive: a second call returns nil until another notice is
// recorded. Implements runtime.NoticeSource structurally (this package does
// not import runtime).
func (b *noticeBuffer) DrainNotices() []string {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.notices
	b.notices = nil
	return out
}
