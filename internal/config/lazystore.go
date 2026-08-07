package config

import (
	"fmt"
	"io"
	"os"
	"sync"
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
	return &lazyStore{resolve: func() (CredStore, error) {
		store, notice, err := openStoreResolved(p, dir)
		// The lazy path resolves inside a credential operation (SetToken,
		// GetToken, …), outside any pipeline Result — the notice cannot reach
		// an envelope here. Write it to stderr directly in # warning: format.
		if notice != "" {
			writeWarningLine(os.Stderr, notice)
		}
		return store, err
	}}, nil
}

func (l *lazyStore) backend() (CredStore, error) {
	l.once.Do(func() { l.store, l.err = l.resolve() })
	return l.store, l.err
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

// writeWarningLine writes a single # warning: line to w. It is the canonical
// format for user-visible notices in this package when a pipeline Result is not
// available (e.g. the lazy-store resolution path). Keeping the format string
// here avoids duplicating the literal across multiple call sites and lets a
// wording change apply uniformly.
func writeWarningLine(w io.Writer, msg string) {
	_, _ = fmt.Fprintf(w, "# warning: %s\n", msg)
}
