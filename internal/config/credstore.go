package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/zalando/go-keyring"
	"sigs.k8s.io/yaml"
)

// ErrNoToken indicates no credential is stored for the server.
var ErrNoToken = errors.New("no stored token")

const keyringService = "rancher-cli"

// CredStore persists per-server Rancher tokens and cached exec credentials.
type CredStore interface {
	GetToken(server string) (string, error)
	SetToken(server, token string) error
	DeleteToken(server string) error
	GetCachedCred(key string) (token string, expiry time.Time, err error)
	SetCachedCred(key, token string, expiry time.Time, name, serverHost, cluster string) error
	DeleteCachedCred(key string) error
	DeleteAllCachedCreds() error
	ListCachedCreds() ([]CachedCredInfo, error)
}

type cachedCred struct {
	Token   string    `json:"token"`
	Expiry  time.Time `json:"expiry"`
	Name    string    `json:"name,omitempty"`
	Server  string    `json:"server,omitempty"`
	Cluster string    `json:"cluster,omitempty"`
}

// CachedCredInfo describes one cached exec-credential entry for listing. Key is
// the composite storage key; Name/Server/Cluster are the display fields (for a
// legacy bare entry Name is the raw key and Server/Cluster are empty).
type CachedCredInfo struct {
	Key     string    `json:"key"`
	Name    string    `json:"name"`
	Server  string    `json:"server,omitempty"`
	Cluster string    `json:"cluster,omitempty"`
	Expiry  time.Time `json:"expiry,omitempty"`
}

// CacheKey scopes an exec-credential cache entry by server host and cluster, so
// the same --name used against two servers never collides.
func CacheKey(name, serverHost, cluster string) string {
	return name + "@" + serverHost + "/" + cluster
}

// fileStore keeps tokens in a 0600 YAML map file.
type fileStore struct {
	path      string
	cachePath string
	mu        sync.Mutex
}

func NewFileStore(dir string) CredStore {
	return &fileStore{
		path:      filepath.Join(dir, "credentials.yaml"),
		cachePath: filepath.Join(dir, "token-cache.yaml"),
	}
}

// warnedLoosePerm dedupes the loose-permission warning: read paths run on every
// credential access, so without this the same warning would repeat all run.
var warnedLoosePerm sync.Map // path -> struct{}

// warnLoosePerm notices on stderr when a credential file is group/world
// readable, so a plaintext token in a 0644 file does not sit unremarked (CS-1).
// It warns at most once per path per process.
func warnLoosePerm(path string) {
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		if _, seen := warnedLoosePerm.LoadOrStore(path, struct{}{}); !seen {
			logrus.Warnf("%s is group/world-readable (%o); run: chmod 600 %s", path, fi.Mode().Perm(), path)
		}
	}
}

func (f *fileStore) read() (map[string]string, error) {
	m := map[string]string{}
	b, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	warnLoosePerm(f.path)
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (f *fileStore) write(m map[string]string) error {
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(f.path, b, 0600)
}

func (f *fileStore) GetToken(server string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return "", err
	}
	tok, ok := m[server]
	if !ok {
		return "", ErrNoToken
	}
	return tok, nil
}

func (f *fileStore) SetToken(server, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return err
	}
	m[server] = token
	return f.write(m)
}

func (f *fileStore) DeleteToken(server string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return err
	}
	delete(m, server)
	return f.write(m)
}

func (f *fileStore) readCache() (map[string]cachedCred, error) {
	m := map[string]cachedCred{}
	b, err := os.ReadFile(f.cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	warnLoosePerm(f.cachePath)
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (f *fileStore) writeCache(m map[string]cachedCred) error {
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(f.cachePath, b, 0600)
}

func (f *fileStore) GetCachedCred(key string) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.readCache()
	if err != nil {
		return "", time.Time{}, err
	}
	cred, ok := m[key]
	if !ok {
		return "", time.Time{}, ErrNoToken
	}
	if !cred.Expiry.IsZero() && time.Now().After(cred.Expiry) {
		delete(m, key)
		if werr := f.writeCache(m); werr != nil {
			return "", time.Time{}, werr
		}
		return "", time.Time{}, ErrNoToken
	}
	return cred.Token, cred.Expiry, nil
}

func (f *fileStore) SetCachedCred(key, token string, expiry time.Time, name, serverHost, cluster string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.readCache()
	if err != nil {
		return err
	}
	m[key] = cachedCred{Token: token, Expiry: expiry, Name: name, Server: serverHost, Cluster: cluster}
	return f.writeCache(m)
}

func (f *fileStore) DeleteCachedCred(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.readCache()
	if err != nil {
		return err
	}
	delete(m, key)
	return f.writeCache(m)
}

func (f *fileStore) DeleteAllCachedCreds() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := os.Remove(f.cachePath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (f *fileStore) ListCachedCreds() ([]CachedCredInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.readCache()
	if err != nil {
		return nil, err
	}
	out := make([]CachedCredInfo, 0, len(m))
	pruned := false
	for key, cred := range m {
		if !cred.Expiry.IsZero() && time.Now().After(cred.Expiry) {
			delete(m, key)
			pruned = true
			continue
		}
		info := CachedCredInfo{
			Key:     key,
			Name:    cred.Name,
			Server:  cred.Server,
			Cluster: cred.Cluster,
			Expiry:  cred.Expiry,
		}
		if info.Name == "" {
			// Legacy bare entry: no record fields — surface the raw key as the name.
			info.Name = key
		}
		out = append(out, info)
	}
	if pruned {
		// Best-effort prune: expired entries are already excluded from out; a failed
		// write-back just leaves them on disk for a later pass. Never fail a list for it.
		_ = f.writeCache(m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// keyringStore keeps long-lived login tokens in the OS keychain, but forwards
// the ephemeral exec-cred cache to a file store — the keychain has no list API,
// and a single file is an enumerable single source of truth.
type keyringStore struct {
	cache   CredStore
	pinHint string // non-empty when selected via the auto pin (Task 2 uses it)
}

func NewKeyringStore(dir string) CredStore { return newPinnedKeyringStore(dir, false) }

// newPinnedKeyringStore builds a keyring-backed store. When pinned is true the
// store carries a remediation hint appended to every keyring error, so a
// pinned-but-unusable keychain fails loudly with a way out instead of silently.
func newPinnedKeyringStore(dir string, pinned bool) CredStore {
	hint := ""
	if pinned {
		hint = fmt.Sprintf(" (credential store pinned to keychain at %s; delete the pin to re-probe, or set RANCHER_CREDENTIAL_STORE=file)", PinPath(dir))
	}
	return &keyringStore{cache: NewFileStore(dir), pinHint: hint}
}

func (k *keyringStore) GetToken(server string) (string, error) {
	tok, err := keyring.Get(keyringService, server)
	if errors.Is(err, keyring.ErrNotFound) {
		// Read-through: tokens stored under an earlier "file" pin keep
		// working after an upgrade re-pin; they age out via logout/re-login.
		return k.cache.GetToken(server)
	}
	if err != nil {
		return "", fmt.Errorf("keyring get %q: %w%s", server, err, k.pinHint)
	}
	return tok, nil
}

func (k *keyringStore) SetToken(server, token string) error {
	if err := keyring.Set(keyringService, server, token); err != nil {
		return fmt.Errorf("keyring set %q: %w%s", server, err, k.pinHint)
	}
	return nil
}

func (k *keyringStore) DeleteToken(server string) error {
	err := keyring.Delete(keyringService, server)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("keyring delete %q: %w%s", server, err, k.pinHint)
	}
	// Read-through symmetry with GetToken: a token may live in the file store
	// under an earlier "file" pin, so clear it here too (absent is not an error).
	if ferr := k.cache.DeleteToken(server); ferr != nil && !errors.Is(ferr, ErrNoToken) {
		return ferr
	}
	return nil
}

func (k *keyringStore) GetCachedCred(key string) (string, time.Time, error) {
	return k.cache.GetCachedCred(key)
}
func (k *keyringStore) SetCachedCred(key, token string, expiry time.Time, name, serverHost, cluster string) error {
	return k.cache.SetCachedCred(key, token, expiry, name, serverHost, cluster)
}
func (k *keyringStore) DeleteCachedCred(key string) error          { return k.cache.DeleteCachedCred(key) }
func (k *keyringStore) DeleteAllCachedCreds() error                { return k.cache.DeleteAllCachedCreds() }
func (k *keyringStore) ListCachedCreds() ([]CachedCredInfo, error) { return k.cache.ListCachedCreds() }

// DrainToken deletes a server's login token from BOTH backends, unconditionally
// and independently of policy: read-through made the file store part of the
// effective read path under every policy, so every logout must drain it, and
// the keychain may still hold a token from before a backend flip. Neither
// backend's failure short-circuits the other; per-backend errors are joined so
// a live token is never left behind unreported. deletedFrom lists the backends
// a token was actually removed from.
func DrainToken(dir, server string) ([]string, error) {
	var (
		from []string
		errs []error
	)
	if err := keyring.Delete(keyringService, server); err == nil {
		from = append(from, "keychain")
	} else if !errors.Is(err, keyring.ErrNotFound) {
		errs = append(errs, fmt.Errorf("keyring delete %q: %w", server, err))
	}
	fs := NewFileStore(dir)
	switch _, err := fs.GetToken(server); {
	case err == nil:
		if derr := fs.DeleteToken(server); derr != nil {
			errs = append(errs, fmt.Errorf("file credential store: %w", derr))
		} else {
			from = append(from, "file")
		}
	case errors.Is(err, ErrNoToken):
		// nothing stored in the file backend — fine
	default:
		// NEVER report a successful drain when the file store is
		// unreadable/corrupt: a live plaintext token may remain.
		errs = append(errs, fmt.Errorf("file credential store: %w", err))
	}
	return from, errors.Join(errs...)
}

// OpenStore selects a store by policy. Explicit policies ("keychain"/"file")
// are honored directly and never consult the pin. "auto" reads the pin; when
// unpinned it probes ONCE, pins the outcome, and notices on stderr — loudly
// when the outcome is plaintext. Unknown policies are hard errors (CS-6).
func OpenStore(policy, dir string) (CredStore, error) {
	p := EffectivePolicy(policy)
	switch p {
	case "file":
		return NewFileStore(dir), nil
	case "keychain":
		return newPinnedKeyringStore(dir, false), nil
	case "auto":
		switch ReadPin(dir) {
		case "keychain":
			return newPinnedKeyringStore(dir, true), nil
		case "file":
			return NewFileStore(dir), nil
		}
		if keyringUsable() {
			if err := WritePin(dir, "keychain"); err != nil {
				return nil, fmt.Errorf("writing credential store pin: %w", err)
			}
			logrus.Warnf("pinned credential store: keychain")
			return newPinnedKeyringStore(dir, true), nil
		}
		if err := WritePin(dir, "file"); err != nil {
			return nil, fmt.Errorf("writing credential store pin: %w", err)
		}
		logrus.Warnf("keychain unavailable; credentials will be stored in plaintext %s", filepath.Join(dir, "credentials.yaml"))
		return NewFileStore(dir), nil
	default:
		// Format the EFFECTIVE value p, not the cfg argument — when an invalid
		// RANCHER_CREDENTIAL_STORE overrides an empty/valid config value, the
		// error must name what was actually rejected.
		return nil, fmt.Errorf("unknown credential store %q (valid: keychain, file, auto)", p)
	}
}

// keyringProbeTimeout bounds keyringUsable: on macOS the underlying `security`
// exec can block indefinitely without a usable keychain session (headless/SSH/
// sandbox), and a hung probe must degrade to the file store, not hang login.
// Package var so tests can shrink it.
var keyringProbeTimeout = 5 * time.Second

// keyringProbe does the harmless set/delete of a marker key. Package var so
// tests can stub it with a never-returning body to exercise the timeout.
var keyringProbe = func() bool {
	const probe = "__rancher_cli_probe__"
	if err := keyring.Set(keyringService, probe, "1"); err != nil {
		return false
	}
	_ = keyring.Delete(keyringService, probe)
	return true
}

// keyringUsable runs keyringProbe bounded to keyringProbeTimeout, treating a
// timeout as unusable. The probe goroutine may outlive the timeout (the exec
// is not cancellable) — acceptable for a once-per-login probe. probe/timeout
// are snapshotted before the goroutine starts so a test restoring the package
// vars right after the timeout fires never races with the leaked goroutine's
// read of them. Package var so tests can count/stub probes.
var keyringUsable = func() bool {
	probe, timeout := keyringProbe, keyringProbeTimeout
	done := make(chan bool, 1)
	go func() { done <- probe() }()
	select {
	case ok := <-done:
		return ok
	case <-time.After(timeout):
		return false
	}
}
