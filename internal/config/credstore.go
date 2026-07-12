package config

import (
	"errors"
	"fmt"
	"net/url"
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
// It warns at most once per path per process. On Windows this Unix-perm
// check is a documented no-op (NTFS ignores mode bits; Mode().Perm() is
// synthetic there) — the owner-only DACL set by hardenACL at write time
// is the guarantee on that platform.
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

// newPinnedKeyringStore builds a keyring-backed store. When pinned is true the
// store carries a remediation hint appended to every keyring error, so a
// pinned-but-unusable keychain fails loudly with a way out instead of silently.
func newPinnedKeyringStore(dir string, pinned bool) CredStore {
	hint := ""
	if pinned {
		hint = fmt.Sprintf(" (credential store pinned to keychain at %s; delete the pin to re-probe, or set %s=file)", PinPath(dir), EnvCredentialStore)
	}
	return &keyringStore{cache: NewFileStore(dir), pinHint: hint}
}

// keyringUnreachable reports whether err is a backend-unreachable keyring
// failure — a transport/connection-class error (Linux: godbus "no session
// bus" on headless; macOS: locked/unavailable Keychain; Windows:
// Credential Manager transport) — as opposed to a clean miss
// (keyring.ErrNotFound, which is NOT unreachable). go-keyring exposes no
// typed transport errors, so any non-miss failure is treated as
// unreachable; for reads that errs toward resilience, and the write path
// hard-fails either way.
func keyringUnreachable(err error) bool {
	return err != nil && !errors.Is(err, keyring.ErrNotFound)
}

func (k *keyringStore) GetToken(server string) (string, error) {
	tok, err := keyring.Get(keyringService, server)
	if err == nil {
		return tok, nil
	}
	if errors.Is(err, keyring.ErrNotFound) {
		// Read-through: tokens stored under an earlier "file" pin keep
		// working after an upgrade re-pin; they age out via logout/re-login.
		return k.cache.GetToken(server)
	}
	// Backend unreachable (XP-6): degrade the READ to the file store — a
	// valid token in credentials.yaml must keep working on a machine whose
	// pinned keychain is temporarily unusable (headless SSH, locked
	// keychain). Reads create no new plaintext, so this is pure resilience;
	// the WRITE path (SetToken) deliberately does not degrade.
	ftok, ferr := k.cache.GetToken(server)
	switch {
	case ferr == nil:
		return ftok, nil
	case errors.Is(ferr, ErrNoToken):
		// Double miss: surface the keyring error, not ErrNoToken — the
		// token may exist in the unreachable keychain, and "no stored
		// token" would misread as logged-out.
		return "", fmt.Errorf("keyring get %q: %w%s", server, err, k.pinHint)
	default:
		// The file store FAILED (malformed/unreadable credentials.yaml) —
		// a distinct problem the user must see; don't mask it as a
		// keychain-only issue.
		return "", fmt.Errorf("keyring get %q: %w%s; file credential store: %w", server, err, k.pinHint, ferr)
	}
}

func (k *keyringStore) SetToken(server, token string) error {
	err := keyring.Set(keyringService, server, token)
	if err == nil {
		return nil
	}
	if keyringUnreachable(err) {
		// XP-6 write path: NEVER silently degrade a login write to the
		// plaintext file — the pin's guarantee is that a transient keychain
		// failure cannot downgrade the machine's storage. Refuse with the
		// explicit opt-in remedy instead.
		return fmt.Errorf(
			"keyring set %q: %w — credential keychain is unreachable (locked keychain? no D-Bus session?); the token was NOT stored; set %s=file to store credentials in a permission-restricted file instead, or restore keychain access%s",
			server, err, EnvCredentialStore, k.pinHint)
	}
	return fmt.Errorf("keyring set %q: %w%s", server, err, k.pinHint)
}

func (k *keyringStore) DeleteToken(server string) error {
	err := keyring.Delete(keyringService, server)
	if keyringUnreachable(err) {
		// XP-6: treat unreachable like a miss — drain the file store below
		// (removing plaintext is strictly safe) — but say so: the keychain
		// copy, if any, could not be checked. auth logout's DrainLoginToken
		// independently surfaces keyring errors, so nothing is silently
		// left behind on that path.
		logrus.Warnf("keyring delete %q: %v — keychain unreachable, drained the file store only%s", server, err, k.pinHint)
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

// DrainToken is single-server logout's drain point (ARCHITECTURE.md): it
// deletes a server's login token from BOTH backends, unconditionally and
// independently of policy — read-through made the file store part of the
// effective read path under every policy, so every logout must drain it, and
// the keychain may still hold a token from before a backend flip — AND
// drains every exec-credential cache entry cached against serverURL's host
// (the cache is file-backed regardless of policy, CS-5). No backend's
// failure short-circuits another; per-backend errors are joined so a live
// token/cache entry is never left behind unreported. fromBackends lists the
// login-token backends actually drained; drainedCache lists the cache keys
// actually removed.
//
// Draining the WHOLE host's cache (not just entries matching server's own
// name) is deliberate fail-safe behavior, not an oversight: a cache entry
// embeds the config-entry name only by kubeconfig naming convention, so
// matching by name is structurally unreliable, while over-draining a
// neighbor account on the same host costs it one silent re-auth on next use
// (the exec plugin re-mints transparently) — under-draining leaves a live
// credential behind after the user asked to log out, the exact failure this
// function exists to prevent.
//
// `logout --all` does NOT loop this per server: draining N servers' worth of
// host-scoped cache entries one at a time costs O(servers × cache entries)
// of the same file, and — unlike a single-server logout, which has no other
// way to reach a cache entry cached against a server no longer configured —
// still misses orphaned/bare entries that match no configured server. See
// DrainAllExecCache, which `--all` calls once instead.
func DrainToken(dir, server, serverURL string) (fromBackends, drainedCache []string, err error) {
	fromBackends, err = DrainLoginToken(dir, server)
	drainedCache, cerr := DrainExecCacheForServer(NewFileStore(dir), serverURL)
	if cerr != nil {
		err = errors.Join(err, fmt.Errorf("exec-credential cache: %w", cerr))
	}
	return fromBackends, drainedCache, err
}

// DrainLoginToken deletes a server's login token from both backends,
// unconditionally and independently of policy (see DrainToken). Used
// directly by DrainToken (single-server logout) and by `logout --all`
// (which drains every configured server's token this way, then wipes the
// exec-credential cache once via DrainAllExecCache instead of per server).
func DrainLoginToken(dir, server string) ([]string, error) {
	var (
		from []string
		errs []error
	)
	if kerr := keyring.Delete(keyringService, server); kerr == nil {
		from = append(from, "keychain")
	} else if !errors.Is(kerr, keyring.ErrNotFound) {
		errs = append(errs, fmt.Errorf("keyring delete %q: %w", server, kerr))
	}
	fs := NewFileStore(dir)
	switch _, terr := fs.GetToken(server); {
	case terr == nil:
		if derr := fs.DeleteToken(server); derr != nil {
			errs = append(errs, fmt.Errorf("file credential store: %w", derr))
		} else {
			from = append(from, "file")
		}
	case errors.Is(terr, ErrNoToken):
		// nothing stored in the file backend — fine
	default:
		// NEVER report a successful drain when the file store is
		// unreadable/corrupt: a live plaintext token may remain.
		errs = append(errs, fmt.Errorf("file credential store: %w", terr))
	}
	return from, errors.Join(errs...)
}

// DrainAllExecCache wipes the ENTIRE exec-credential cache file in one
// operation and returns the keys it removed — the cheap, complete
// counterpart to DrainToken's per-server host-scoped drain, used by `logout
// --all`. A single full-file wipe, rather than N per-server host-scoped
// drains, is both cheaper (one read-and-truncate instead of
// O(servers × cache entries)) and more complete: it also catches orphaned
// entries (a legacy bare entry with no Server field, or one cached against a
// server later removed from config.yaml) that no per-server drain — keyed to
// a currently-configured server's host — could ever reach.
func DrainAllExecCache(dir string) ([]string, error) {
	fs := NewFileStore(dir)
	cached, err := fs.ListCachedCreds()
	if err != nil {
		return nil, err
	}
	if err := fs.DeleteAllCachedCreds(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(cached))
	for _, c := range cached {
		keys = append(keys, c.Key)
	}
	return keys, nil
}

// DrainExecCacheForServer deletes every exec-credential cache entry whose
// Server matches serverURL's host — the same host CacheKey embeds when the
// entry was cached — and returns the deleted entries' keys. Entries cached
// against a different server are left alone.
func DrainExecCacheForServer(store CredStore, serverURL string) ([]string, error) {
	host := ServerHost(serverURL)
	if host == "" {
		return nil, nil
	}
	creds, err := store.ListCachedCreds()
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, c := range creds {
		if c.Server != host {
			continue
		}
		if err := store.DeleteCachedCred(c.Key); err != nil {
			return deleted, err
		}
		deleted = append(deleted, c.Key)
	}
	return deleted, nil
}

// ServerHost reduces a server URL to its host, matching CacheKey's
// composition; it falls back to the raw string when it does not parse to a
// host. Shared by every "extract host from a server URL" need (the
// exec-credential cache key, get-token's cache key, kubeconfig's
// env-only entry name) so a future host-extraction rule change (e.g. IPv6
// handling) lands in one place.
func ServerHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// OpenStore selects a store by policy. policy is the config-file-level
// declaration; RANCHER_CLI_CREDENTIAL_STORE, when set, outranks it — same
// precedence as everywhere else env beats config — so an explicit "keychain"
// argument here can still resolve to the file backend under that env var.
// Explicit policies ("keychain"/"file") are honored directly and never
// consult the pin. "auto" reads the pin; when unpinned it probes ONCE, pins
// the outcome, and notices on stderr — loudly when the outcome is plaintext.
// Unknown policies are hard errors (CS-6).
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
		if keyringUsableOnce() {
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
		// RANCHER_CLI_CREDENTIAL_STORE overrides an empty/valid config value, the
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

var (
	keyringUsableOnceGuard sync.Once
	keyringUsableCached    bool
)

// keyringUsableOnce runs keyringUsable at most once per process and caches
// the result. A CLI invocation is one process: OpenStore's auto-probe and
// login's RepinForLogin re-probe would otherwise pay the (bounded,
// keyringProbeTimeout-worst-case) probe cost twice sequentially on an
// unusable keychain. keyringUsable itself stays the swappable seam for tests
// that exercise it directly (e.g. the timeout test); callers that go through
// OpenStore/RepinForLogin observe the memoized result.
func keyringUsableOnce() bool {
	keyringUsableOnceGuard.Do(func() { keyringUsableCached = keyringUsable() })
	return keyringUsableCached
}

// resetKeyringUsableCache clears keyringUsableOnce's memoized result. Tests
// that stub keyringUsable and exercise the probe through OpenStore or
// RepinForLogin must call this first, so their own stub is actually invoked
// rather than a cached result left over from an earlier test in the same
// process.
func resetKeyringUsableCache() {
	keyringUsableOnceGuard = sync.Once{}
}
