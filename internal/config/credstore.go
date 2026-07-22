package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/sirupsen/logrus"
	"github.com/zalando/go-keyring"
	"sigs.k8s.io/yaml"
)

// ErrNoToken indicates no credential is stored for the server.
var ErrNoToken = errors.New("no stored token")

const keyringService = "rancher-cli"

// keyringObserverFn is an optional test hook called on every keyring operation
// performed through keyringStore (Get, Set, Delete). Nil by default — no-op in
// production. Set via SetKeyringObserverForTest to count keyring calls from any
// test, including tests in other packages (e.g. surface/cli_test).
var keyringObserverFn func(op string)

// SetKeyringObserverForTest sets the keyring-operation observer for the current
// test run. Call with nil to disable. op values: "get", "set", "delete".
// This function is in non-test code so it is reachable from other packages'
// tests; it is a no-op in production (keyringObserverFn is nil by default).
func SetKeyringObserverForTest(fn func(op string)) { keyringObserverFn = fn }

// ServerRef identifies a server entry at the credential-store boundary: the
// config-entry name (the storage key for file/keychain backends) plus the
// server URL (required by the exec helper protocol, which is keyed on
// canonical URL + name). Callers always have both in scope.
type ServerRef struct {
	Name string
	URL  string
}

// CredStore persists per-server Rancher tokens and cached exec credentials.
type CredStore interface {
	GetToken(ref ServerRef) (string, error)
	SetToken(ref ServerRef, token string) error
	DeleteToken(ref ServerRef) error
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
// check is SKIPPED via permCheckApplicable (NTFS ignores mode bits;
// Mode().Perm() is synthetic there and would false-positive on every
// file) — the owner-only DACL set by hardenACL at write time is the
// guarantee on that platform.
func warnLoosePerm(path string) {
	if !permCheckApplicable() {
		return
	}
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

func (f *fileStore) GetToken(ref ServerRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return "", err
	}
	tok, ok := m[ref.Name]
	if !ok {
		return "", ErrNoToken
	}
	return tok, nil
}

func (f *fileStore) SetToken(ref ServerRef, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return err
	}
	m[ref.Name] = token
	return f.write(m)
}

func (f *fileStore) DeleteToken(ref ServerRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return err
	}
	delete(m, ref.Name)
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
// (keyring.ErrNotFound, which is NOT unreachable) or an oversized-value
// failure (keyring.ErrSetDataTooBig — wincred rejects a Set over 2560
// bytes, macOS's security CLI over 4096 — a data-size problem, not a
// connectivity one). go-keyring exposes no typed transport errors, so any
// other non-miss failure is treated as unreachable; for reads that errs
// toward resilience, and the write path hard-fails either way.
func keyringUnreachable(err error) bool {
	return err != nil && !errors.Is(err, keyring.ErrNotFound) && !errors.Is(err, keyring.ErrSetDataTooBig)
}

func (k *keyringStore) GetToken(ref ServerRef) (string, error) {
	if keyringObserverFn != nil {
		keyringObserverFn("get")
	}
	tok, err := keyring.Get(keyringService, ref.Name)
	if err == nil {
		return tok, nil
	}
	if errors.Is(err, keyring.ErrNotFound) {
		// Read-through: tokens stored under an earlier "file" pin keep
		// working after an upgrade re-pin; they age out via logout/re-login.
		return k.cache.GetToken(ref)
	}
	// Backend unreachable (XP-6): degrade the READ to the file store — a
	// valid token in credentials.yaml must keep working on a machine whose
	// pinned keychain is temporarily unusable (headless SSH, locked
	// keychain). Reads create no new plaintext, so this is pure resilience;
	// the WRITE path (SetToken) deliberately does not degrade.
	ftok, ferr := k.cache.GetToken(ref)
	switch {
	case ferr == nil:
		return ftok, nil
	case errors.Is(ferr, ErrNoToken):
		// Double miss: surface the keyring error, not ErrNoToken — the
		// token may exist in the unreachable keychain, and "no stored
		// token" would misread as logged-out.
		return "", fmt.Errorf("keyring get %q: %w%s", ref.Name, err, k.pinHint)
	default:
		// The file store FAILED (malformed/unreadable credentials.yaml) —
		// a distinct problem the user must see; don't mask it as a
		// keychain-only issue.
		return "", fmt.Errorf("keyring get %q: %w%s; file credential store: %w", ref.Name, err, k.pinHint, ferr)
	}
}

func (k *keyringStore) SetToken(ref ServerRef, token string) error {
	if keyringObserverFn != nil {
		keyringObserverFn("set")
	}
	err := keyring.Set(keyringService, ref.Name, token)
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
			ref.Name, err, EnvCredentialStore, k.pinHint)
	}
	return fmt.Errorf("keyring set %q: %w%s", ref.Name, err, k.pinHint)
}

func (k *keyringStore) DeleteToken(ref ServerRef) error {
	if keyringObserverFn != nil {
		keyringObserverFn("delete")
	}
	err := keyring.Delete(keyringService, ref.Name)
	if keyringUnreachable(err) {
		// XP-6: treat unreachable like a miss — drain the file store below
		// (removing plaintext is strictly safe) — but say so: the keychain
		// copy, if any, could not be checked. auth logout's DrainLoginToken
		// independently surfaces keyring errors, so nothing is silently
		// left behind on that path.
		logrus.Warnf("keyring delete %q: %v — keychain unreachable, drained the file store only%s", ref.Name, err, k.pinHint)
	}
	// Read-through symmetry with GetToken: a token may live in the file store
	// under an earlier "file" pin, so clear it here too (absent is not an error).
	if ferr := k.cache.DeleteToken(ref); ferr != nil && !errors.Is(ferr, ErrNoToken) {
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
func DrainToken(dir string, ref ServerRef, policy string) (fromBackends, drainedCache []string, err error) {
	fromBackends, err = DrainLoginToken(dir, ref, policy)
	drainedCache, cerr := DrainExecCacheForServer(NewFileStore(dir), ref.URL)
	if cerr != nil {
		err = errors.Join(err, fmt.Errorf("exec-credential cache: %w", cerr))
	}
	return fromBackends, drainedCache, err
}

// DrainLoginToken deletes a server's login token from the keychain, file
// store, and every recorded exec-helper provenance entry for the server.
// The exec union covers: the current policy's helper (with the current
// identity) and every older provenance entry — so a policy change between
// login and logout never strands a helper credential. Used directly by
// DrainToken (single-server logout) and by `logout --all` (which drains
// every configured server's token this way, then wipes the exec-credential
// cache once via DrainAllExecCache instead of per server).
func DrainLoginToken(dir string, ref ServerRef, policy string) ([]string, error) {
	var (
		from []string
		errs []error
	)
	if kerr := keyring.Delete(keyringService, ref.Name); kerr == nil {
		from = append(from, "keychain")
	} else if errors.Is(kerr, keyring.ErrNotFound) {
		// nothing stored in the keychain backend — fine
	} else if keyringUnreachable(kerr) {
		// Mirror keyringStore.DeleteToken's XP-6 degrade: an unreachable
		// keychain must not turn `auth logout` into a hard failure when
		// the file store still drains cleanly.
		logrus.Warnf("keyring delete %q: %v — keychain unreachable, drained the file store only", ref.Name, kerr)
	} else {
		errs = append(errs, fmt.Errorf("keyring delete %q: %w", ref.Name, kerr))
	}
	fs := NewFileStore(dir)
	switch _, terr := fs.GetToken(ref); {
	case terr == nil:
		if derr := fs.DeleteToken(ref); derr != nil {
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

	// Exec helpers: erase through the union of the current policy's helper
	// (with the current identity) and every recorded provenance entry for
	// this server — a policy change between login and logout must not strand
	// a helper credential. The provenance lock spans erase + unrecord.
	targets := map[ProvenanceEntry]struct{}{}
	if name, ok := execHelperName(policy); ok {
		if !credhelper.ValidHelperName(name) {
			errs = append(errs, fmt.Errorf("invalid credential helper name %q in effective policy; refusing to execute it", name))
		} else if cu, cerr := credhelper.CanonicalServerURL(ref.URL); cerr == nil {
			targets[ProvenanceEntry{Helper: name, Name: ref.Name, CanonicalURL: cu}] = struct{}{}
		} else if ref.URL != "" {
			errs = append(errs, fmt.Errorf("credential helper drain: %w", cerr))
		}
	}
	lerr := withProvenanceLock(dir, func(st *provenanceState) error {
		for _, e := range st.forServer(ref.Name) {
			targets[e] = struct{}{}
		}
		var eerrs []error
		for e := range targets {
			es := newExecStore(e.Helper, dir)
			if _, ierr := es.invoke(credhelper.VerbErase, credhelper.Request{ServerURL: e.CanonicalURL, Name: e.Name}); ierr != nil {
				eerrs = append(eerrs, fmt.Errorf("credential helper %q erase: %w", e.Helper, ierr))
				continue // entry stays for retry
			}
			st.remove(ProvenanceEntry{Helper: e.Helper, Name: e.Name, CanonicalURL: e.CanonicalURL})
			from = append(from, "helper:"+e.Helper)
		}
		return errors.Join(eerrs...)
	})
	if lerr != nil {
		errs = append(errs, lerr)
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
//
// The target host is canonicalized via CanonicalServerHost. Legacy stored
// records (entries where Server carries a non-canonical spelling such as
// HOST:443 or HOST:80) are normalized using the TARGET's scheme, so that a
// default-port suffix is stripped correctly for each scheme: HOST:80 drains
// for http://host and HOST:443 drains for https://host, but HOST:80 does NOT
// drain for https://host (different server).
func DrainExecCacheForServer(store CredStore, serverURL string) ([]string, error) {
	host := CanonicalServerHost(serverURL)
	if host == "" {
		return nil, nil
	}
	// Derive the target scheme for normalizing legacy stored records whose
	// Server field carries no scheme. Default to https to preserve
	// previous behavior for malformed target URLs.
	scheme := "https"
	if u, err := url.Parse(serverURL); err == nil && u.Scheme != "" {
		scheme = strings.ToLower(u.Scheme)
	}
	creds, err := store.ListCachedCreds()
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, c := range creds {
		if c.Server == "" || CanonicalServerHost(scheme+"://"+c.Server) != host {
			continue
		}
		if err := store.DeleteCachedCred(c.Key); err != nil {
			return deleted, err
		}
		deleted = append(deleted, c.Key)
	}
	return deleted, nil
}

// CanonicalServerHost derives the canonical cache-identity host from a full
// server URL: the host lowercased and stripped of its scheme's default port
// (443 for https, 80 for http). Falls back to a lowercased ServerHost when
// the URL cannot be canonicalized, so callers always get a non-empty string
// for a non-empty URL. Use this wherever an exec-credential cache key is
// generated or compared; use ServerHost for non-cache identity (e.g.
// kubeconfig entry names).
func CanonicalServerHost(rawURL string) string {
	if cu, err := credhelper.CanonicalServerURL(rawURL); err == nil {
		if u, err := url.Parse(cu); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return strings.ToLower(ServerHost(rawURL))
}

// ServerHost reduces a server URL to its host; falls back to the raw string
// when it does not parse to a host. Use for non-cache-identity needs (e.g.
// kubeconfig env-entry naming). For exec-credential cache key generation, use
// CanonicalServerHost so that equivalent URL spellings produce the same key.
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
	return openStoreResolved(EffectivePolicy(policy), dir)
}

// execHelperName extracts the helper name from an "exec:<name>" policy value.
func execHelperName(policy string) (string, bool) {
	name, ok := strings.CutPrefix(policy, "exec:")
	if !ok {
		return "", false
	}
	return name, true
}

// checkPolicy returns CS-6's error for an unknown effective policy value p.
// Both openStoreResolved and NewLazyStore call this so the valid-policy
// allowlist is defined exactly once — adding a new backend means updating
// this one allowlist — both paths share it, so they cannot silently diverge.
func checkPolicy(p string) error {
	if name, ok := execHelperName(p); ok {
		if !credhelper.ValidHelperName(name) {
			return fmt.Errorf("invalid credential helper name %q in %q (must match ^[a-z0-9][a-z0-9-]*$)", name, p)
		}
		return nil
	}
	switch p {
	case "file", "keychain", "auto":
		return nil
	default:
		return fmt.Errorf("unknown credential store %q (valid: keychain, file, auto, exec:<name>)", p)
	}
}

// openStoreResolved is OpenStore's body operating on an ALREADY-EFFECTIVE
// policy value: the lazy store validates and captures the effective policy
// at construction, and deferring through this function guarantees the
// policy opened is the policy validated even if the environment changes
// between construction and first use.
func openStoreResolved(p, dir string) (CredStore, error) {
	if err := checkPolicy(p); err != nil {
		return nil, err
	}
	if name, ok := execHelperName(p); ok {
		return newExecStore(name, dir), nil
	}
	switch p {
	case "file":
		return NewFileStore(dir), nil
	case "keychain":
		return newPinnedKeyringStore(dir, false), nil
	default: // "auto"
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
