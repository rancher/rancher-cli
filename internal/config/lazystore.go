package config

import (
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
	return &lazyStore{resolve: func() (CredStore, error) { return openStoreResolved(p, dir) }}, nil
}

func (l *lazyStore) backend() (CredStore, error) {
	l.once.Do(func() { l.store, l.err = l.resolve() })
	return l.store, l.err
}

func (l *lazyStore) GetToken(server string) (string, error) {
	s, err := l.backend()
	if err != nil {
		return "", err
	}
	return s.GetToken(server)
}

func (l *lazyStore) SetToken(server, token string) error {
	s, err := l.backend()
	if err != nil {
		return err
	}
	return s.SetToken(server, token)
}

func (l *lazyStore) DeleteToken(server string) error {
	s, err := l.backend()
	if err != nil {
		return err
	}
	return s.DeleteToken(server)
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
