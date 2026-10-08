package config

import (
	"sync"
	"sync/atomic"
	"time"
)

// lazyStore defers backend resolution (the pin read and, for an `auto`
// directory with no pin, the look at what its entries have stored) to the
// FIRST credential method call, so a command that never touches
// credentials reads nothing it does not need and creates nothing - no
// config-dir creation.
// resolve runs at most once; its result (or error) is cached for the
// process lifetime and returned as-is from every call (callers keep their
// existing error wrapping).
type lazyStore struct {
	resolve func() (CredStore, error)
	once    sync.Once
	store   CredStore
	err     error
	// resolved flips true (via atomic.Bool, not a plain bool guarded by
	// once alone) only after store/err are fully written, so DrainNotices
	// can safely read them from any goroutine without forcing resolution
	// itself - checking resolved is a query, never a trigger.
	resolved atomic.Bool
}

// NewLazyStore validates the EFFECTIVE policy of cfg's declaration eagerly
// (an unknown RANCHER_CLI_CREDENTIAL_STORE or config credentialStore still
// fails fast on every resolveDeps-backed command) and captures it, so
// the deferred resolution provably opens the policy that was validated even
// if the environment changes mid-process. cfg is the config the
// command loaded: its entries are what an unpinned `auto` directory is
// judged by, so resolution loads nothing of its own.
//
// A declaration the variable disagrees with is the one resolution failure
// that is NOT eager: it is carried into the deferred resolution and
// surfaces at the first credential call, so a command that never reads a
// credential - help, schema, completion of a non-credential argument,
// `config path`, a RANCHER_CLI_TOKEN run - is not failed by a conflict it
// would never have acted on. An unknown policy value stays eager: it is
// broken for every command, whether or not one opens the store.
func NewLazyStore(cfg *Config, dir string) (CredStore, error) {
	p, conflict := EffectivePolicy(cfg.CredentialStore)
	if err := checkPolicy(p); err != nil {
		return nil, err
	}
	if conflict != nil {
		return &lazyStore{resolve: func() (CredStore, error) {
			return nil, conflict
		}}, nil
	}
	return &lazyStore{resolve: func() (CredStore, error) {
		return openStoreResolved(p, dir, func() (*Config, error) { return cfg, nil })
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
// store-init side effects even though the pipeline always drains
// before rendering. Once resolution has happened (by any caller, at any
// point before this call), the underlying backend's own notices - a
// loose-permission finding, say - are drained.
func (l *lazyStore) DrainNotices() []string {
	if !l.resolved.Load() {
		return nil
	}
	if ns, ok := l.store.(NoticeSource); ok {
		return ns.DrainNotices()
	}
	return nil
}

// ResolvedBackend implements CredStore without forcing resolution: the
// store a lazy store reads from is the one its first credential call
// fixed, and before that call it has read from none. A resolution that
// failed fixed no backend.
func (l *lazyStore) ResolvedBackend() (string, bool) {
	switch {
	case !l.resolved.Load():
		return "", false
	case l.err != nil:
		return "", true
	}
	return l.store.ResolvedBackend()
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

// SetTokenWithOutcome resolves the backend and delegates, prior included:
// only the resolved backend knows what to do with it. A backend that fails
// to resolve wrote nothing, and names no store to attribute that to.
func (l *lazyStore) SetTokenWithOutcome(ref ServerRef, token string, prior PriorCredential) (CommitOutcome, error) {
	s, err := l.backend()
	if err != nil {
		return CommitOutcome{State: CommitNotWritten}, err
	}
	return s.SetTokenWithOutcome(ref, token, prior)
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

func (l *lazyStore) GetCachedCred(key string, anchor *CacheAnchor) (string, time.Time, error) {
	s, err := l.backend()
	if err != nil {
		return "", time.Time{}, err
	}
	return s.GetCachedCred(key, anchor)
}

func (l *lazyStore) GetCachedCredDetail(key string, anchor *CacheAnchor) (CachedCredDetail, error) {
	s, err := l.backend()
	if err != nil {
		return CachedCredDetail{}, err
	}
	return s.GetCachedCredDetail(key, anchor)
}

func (l *lazyStore) SetCachedCred(key, token string, expiry time.Time, name, serverHost, cluster string, binding CacheBinding) error {
	s, err := l.backend()
	if err != nil {
		return err
	}
	return s.SetCachedCred(key, token, expiry, name, serverHost, cluster, binding)
}

func (l *lazyStore) MarkCachedCredReady(key, token, fingerprint string) (bool, error) {
	s, err := l.backend()
	if err != nil {
		return false, err
	}
	return s.MarkCachedCredReady(key, token, fingerprint)
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
