package config

import (
	"sync"
	"sync/atomic"
	"time"
)

// lazyStore defers backend resolution (the auto probe, the credstore-pin
// write, the plaintext notice) to the FIRST credential method call, so a
// command that never touches credentials triggers no store-init side
// effects — no probe latency, no pin, no config-dir creation (LS-1).
// resolve runs at most once; its result (or error) is cached for the
// process lifetime and returned as-is from every call (callers keep their
// existing error wrapping, spec R1-3).
type lazyStore struct {
	resolve func() (CredStore, error)
	once    sync.Once
	store   CredStore
	err     error
	// resolved flips true (via atomic.Bool, not a plain bool guarded by
	// once alone) only after store/err are fully written, so DrainNotices
	// can safely read them from any goroutine without forcing resolution
	// itself — checking resolved is a query, never a trigger.
	resolved atomic.Bool
	// notices holds the first-pin/plaintext-fallback notice recorded during
	// resolution — a lazyStore is constructed once per invocation, never
	// shared with another store, so it owns this buffer outright (contrast
	// keyringStore, which shares one with its wrapped file cache).
	notices *noticeBuffer
}

// NewLazyStore validates the EFFECTIVE policy eagerly (an unknown
// RANCHER_CLI_CREDENTIAL_STORE or config credentialStore still fails fast
// on every resolveDeps-backed command, CS-6) and captures it, so the
// deferred resolution provably opens the policy that was validated even if
// the environment changes mid-process (spec R1-4).
func NewLazyStore(policy, dir string) (CredStore, error) {
	p := EffectivePolicy(policy)
	if err := checkPolicy(p); err != nil {
		return nil, err
	}
	nb := &noticeBuffer{}
	return &lazyStore{notices: nb, resolve: func() (CredStore, error) {
		store, notice, err := openStoreResolved(p, dir)
		// The lazy path resolves inside a credential operation (SetToken,
		// GetToken, …), outside any pipeline Result — record it on this
		// store's own buffer instead of writing to stderr directly; the
		// invocation's NoticeSource drain (renderErr/renderResult) picks it
		// up wherever this store is wired onto IO.Notices.
		nb.record(notice)
		return store, err
	}}, nil
}

func (l *lazyStore) backend() (CredStore, error) {
	l.once.Do(func() {
		l.store, l.err = l.resolve()
		l.resolved.Store(true)
	})
	return l.store, l.err
}

// DrainNotices implements runtime.NoticeSource. It never forces resolution:
// an invocation that never calls a credential method must see zero
// store-init side effects (LS-1) even though the pipeline always drains
// before rendering. Once resolution has happened (by any caller, at any
// point before this call), the underlying backend's own notices — a later
// loose-permission finding, say, not just the first-pin notice recorded
// during resolution — are drained too, in the order they would naturally
// occur (the pin notice is always recorded before resolved flips true).
func (l *lazyStore) DrainNotices() []string {
	out := l.notices.DrainNotices()
	if l.resolved.Load() {
		if ns, ok := l.store.(NoticeSource); ok {
			out = append(out, ns.DrainNotices()...)
		}
	}
	return out
}

func (l *lazyStore) GetToken(ref ServerRef) (string, error) {
	s, err := l.backend()
	if err != nil {
		return "", err
	}
	return s.GetToken(ref)
}

func (l *lazyStore) SetToken(ref ServerRef, token string) error {
	s, err := l.backend()
	if err != nil {
		return err
	}
	return s.SetToken(ref, token)
}

// SetTokenWithOutcome resolves the backend and delegates. A backend that
// fails to resolve wrote nothing, and names no store to attribute that to.
func (l *lazyStore) SetTokenWithOutcome(ref ServerRef, token string) (CommitOutcome, error) {
	s, err := l.backend()
	if err != nil {
		return CommitOutcome{State: CommitNotWritten}, err
	}
	return s.SetTokenWithOutcome(ref, token)
}

func (l *lazyStore) DeleteToken(ref ServerRef) error {
	s, err := l.backend()
	if err != nil {
		return err
	}
	return s.DeleteToken(ref)
}

// ShortLivedTokenExpiry forwards the optional ShortLivedTokenStore interface
// through the resolved backend, so an exec store reached lazily still reports
// its in-memory short-lived expiry. Returns false when the backend fails to
// resolve or does not implement the interface.
func (l *lazyStore) ShortLivedTokenExpiry(ref ServerRef) (time.Time, bool) {
	s, err := l.backend()
	if err != nil {
		return time.Time{}, false
	}
	if slt, ok := s.(ShortLivedTokenStore); ok {
		return slt.ShortLivedTokenExpiry(ref)
	}
	return time.Time{}, false
}

func (l *lazyStore) GetCachedCred(key string) (string, time.Time, error) {
	s, err := l.backend()
	if err != nil {
		return "", time.Time{}, err
	}
	return s.GetCachedCred(key)
}

func (l *lazyStore) SetCachedCred(key, token string, expiry time.Time, name, serverHost, cluster string) error {
	s, err := l.backend()
	if err != nil {
		return err
	}
	return s.SetCachedCred(key, token, expiry, name, serverHost, cluster)
}

func (l *lazyStore) DeleteCachedCred(key string) error {
	s, err := l.backend()
	if err != nil {
		return err
	}
	return s.DeleteCachedCred(key)
}

func (l *lazyStore) DeleteAllCachedCreds() error {
	s, err := l.backend()
	if err != nil {
		return err
	}
	return s.DeleteAllCachedCreds()
}

func (l *lazyStore) ListCachedCreds() ([]CachedCredInfo, error) {
	s, err := l.backend()
	if err != nil {
		return nil, err
	}
	return s.ListCachedCreds()
}
