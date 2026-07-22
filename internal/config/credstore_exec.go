package config

import (
	"fmt"
	"time"
)

var errExecNotImplemented = fmt.Errorf("exec credential helper: protocol client not yet implemented")

// execStore delegates login-token storage to an external helper binary
// (rancher-credential-<helper>) speaking the credhelper wire protocol, and
// the exec-credential cache to the file store. Protocol client lands with
// the full implementation; until then every token method errors clearly.
type execStore struct {
	helper string
	dir    string
	cache  CredStore
}

func newExecStore(helper, dir string) *execStore {
	return &execStore{helper: helper, dir: dir, cache: NewFileStore(dir)}
}

func (s *execStore) GetToken(ref ServerRef) (string, error)     { return "", errExecNotImplemented }
func (s *execStore) SetToken(ref ServerRef, token string) error { return errExecNotImplemented }
func (s *execStore) DeleteToken(ref ServerRef) error            { return errExecNotImplemented }

func (s *execStore) GetCachedCred(key string) (string, time.Time, error) {
	return s.cache.GetCachedCred(key)
}
func (s *execStore) SetCachedCred(key, token string, expiry time.Time, name, serverHost, cluster string) error {
	return s.cache.SetCachedCred(key, token, expiry, name, serverHost, cluster)
}
func (s *execStore) DeleteCachedCred(key string) error          { return s.cache.DeleteCachedCred(key) }
func (s *execStore) DeleteAllCachedCreds() error                { return s.cache.DeleteAllCachedCreds() }
func (s *execStore) ListCachedCreds() ([]CachedCredInfo, error) { return s.cache.ListCachedCreds() }
