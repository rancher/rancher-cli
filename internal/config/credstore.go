package config

import (
	"context"
	"errors"
	"fmt"
	"github.com/rancher/rancher-cli/internal/clierr"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/zalando/go-keyring"
	"sigs.k8s.io/yaml"
)

// ErrNoToken indicates no credential is stored for the server.
var ErrNoToken = errors.New("no stored token")

// LegacyCachePrefix is the reserved exec-credential cache-slot prefix written
// by the legacy token shim (`rancher token`). The prefix is defined here so
// every site that needs it - internal/token/token.go (native rejection guard),
// internal/token/shim.go (slot construction), internal/cli/completion_token.go
// (completion's ambiguity filter) - shares one constant; a future rename or
// second reserved namespace compiles instead of silently diverging.
const LegacyCachePrefix = "legacy:"

// IsLegacyCacheSlot reports whether name was written by the legacy token shim
// (i.e. it begins with LegacyCachePrefix). Used to exclude shim slots from
// native --name validation and from the completion ambiguity count.
func IsLegacyCacheSlot(name string) bool {
	return strings.HasPrefix(name, LegacyCachePrefix)
}

// UserIDCachePrefix begins the cache key of every exec plugin call with
// --user-id. It is reserved like LegacyCachePrefix: auth get-token refuses
// a --name, and auth login and config add-server an entry name, that
// starts with it, so the key of a call without --user-id, which begins
// with its --name, can never equal the key of a call with one.
const UserIDCachePrefix = "user-id:"

// IsUserIDCacheSlot reports whether name lies in the reserved namespace
// UserIDCachePrefix opens.
func IsUserIDCacheSlot(name string) bool {
	return strings.HasPrefix(name, UserIDCachePrefix)
}

const keyringService = "rancher-cli"

// keyringObserverFn is an optional test hook called on every keyring operation
// performed through keyringStore (Get, Set, Delete). Nil by default - no-op in
// production. Set via SetKeyringObserverForTest to count keyring calls from any
// test, including tests in other packages (e.g. internal/cli_test).
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
	// SetTokenWithOutcome persists token and reports exactly what the
	// backend did, including when it failed. prior is the state a
	// compensation restores, classified by the caller; a backend that does
	// not address credentials by account ignores it.
	SetTokenWithOutcome(ref ServerRef, token string, prior PriorCredential) (CommitOutcome, error)
	DeleteToken(ref ServerRef) error
	// GetCachedCred takes the anchor the hit is judged against: a record
	// whose directory, entry or cache binding id does not match what the
	// reader loaded is a miss, and one the reader can prove is not its
	// own is deleted as cleanup. A reader that cannot judge - a broken
	// config.yaml, or a stamp that disagrees with the config it loaded -
	// misses without deleting anything.
	GetCachedCred(key string, anchor *CacheAnchor) (token string, expiry time.Time, err error)
	// GetCachedCredDetail is GetCachedCred with the record's readiness
	// fingerprints; the same expiry eviction, the same ErrNoToken.
	GetCachedCredDetail(key string, anchor *CacheAnchor) (CachedCredDetail, error)
	// SetCachedCred writes the record with the binding it must later be
	// judged by, taken from the same config load as the credential. A
	// binding whose load showed an entry it is bound to with its cache
	// writes paused writes nothing and returns a *CacheWritesPausedError
	// naming that entry; a binding no constructor built, and one refused
	// on purpose (CacheBinding.Refused), write nothing either.
	SetCachedCred(key, token string, expiry time.Time, name, serverHost, cluster string, binding CacheBinding) error
	// MarkCachedCredReady records fingerprint on key's record only when the
	// record still holds token, under the cache lock, and reports whether
	// it did; a record replaced or deleted by another process is left alone.
	MarkCachedCredReady(key, token, fingerprint string) (bool, error)
	DeleteAllCachedCreds() error
	ListCachedCreds() ([]CachedCredInfo, error)
	// HoldCachedCreds runs fn under the cache lock with every record,
	// deleting nothing; fn calls no store method.
	HoldCachedCreds(fn func([]CachedRecord) error) error
	// DeleteCachedCredsIfUnchanged deletes each record of want that still
	// holds want's token, under the cache lock, and reports what it
	// deleted and which records now hold another token, each list in key
	// order.
	DeleteCachedCredsIfUnchanged(want map[string]string) (deleted, changed []string, err error)
	// ResolvedBackend names the physical store login credentials are read
	// from - file, keychain or exec:<name> - and reports whether one has
	// been fixed: false only for a lazy store before its first credential
	// call. A lazy store whose resolution failed reports true with no
	// backend.
	ResolvedBackend() (backend string, fixed bool)
}

type cachedCred struct {
	Token   string    `json:"token"`
	Expiry  time.Time `json:"expiry"`
	Name    string    `json:"name,omitempty"`
	Server  string    `json:"server,omitempty"`
	Cluster string    `json:"cluster,omitempty"`
	// Version and the fields after it are what make this record
	// judgeable: which directory created it, and what the exec plugin's
	// lookup for its call read (CacheBinding says which record is bound to
	// what). A record written in another shape cannot be judged and is a
	// miss, which is what every record written before this one is.
	Version int    `json:"version,omitempty"`
	Dir     string `json:"dir,omitempty"`
	// Kind names the rule the record was written by, and so what a
	// hit requires: the identified entry's own credential, another record
	// of a call that identified an entry, or a call that identified none.
	Kind string `json:"kind,omitempty"`
	// Identified is the entry the call's --name identified, empty for
	// none.
	Identified string `json:"identified,omitempty"`
	// UserID is the call's --user-id, empty for a call without one. The
	// key carries it too; a record is served only to the call whose key
	// names the same user.
	UserID string `json:"userId,omitempty"`
	// Entry and BindingID are the identified entry and its cache binding
	// id, for a call that identified one.
	Entry     string `json:"entry,omitempty"`
	BindingID string `json:"bindingId,omitempty"`
	// Entries, when present, is a list the reader's own must equal, each
	// entry with its cache binding id: every entry answering for Server
	// for a call that identified none, the plugin login entries answering
	// for it for one that did. Present and empty is a list with no entry.
	Entries *[]cachedEntry `json:"entries,omitempty"`
	// CurrentServer and TokenReuse, when present, are the values the
	// record was written under, which the reader's load must still show.
	CurrentServer *string `json:"currentServer,omitempty"`
	TokenReuse    *bool   `json:"tokenReuse,omitempty"`
	// ReadyEndpoints are the fingerprints of the direct endpoints that
	// have accepted this token; an endpoint absent here is re-probed by
	// the exec plugin before the token is handed to kubectl for it.
	ReadyEndpoints []string `json:"readyEndpoints,omitempty"`
	// TokenRef and TokenUserID name the cached token and its owner as the
	// create response reported them, written only when the exec plugin
	// created the token in the call that cached it. A record without
	// TokenRef is never revoked: nothing proves the CLI created its token.
	TokenRef    *clierr.TokenRef `json:"tokenRef,omitempty"`
	TokenUserID string           `json:"tokenUserId,omitempty"`
}

// cachedEntry is one entry of a cache record's list: its name and the
// cache binding id it had when the record was written.
type cachedEntry struct {
	Name      string `json:"name"`
	BindingID string `json:"bindingId,omitempty"`
}

// CachedCredDetail is one cached exec credential with its readiness.
type CachedCredDetail struct {
	Token          string
	Expiry         time.Time
	ReadyEndpoints []string
}

// CachedCredInfo describes one cached exec-credential entry for listing. Key is
// the composite storage key; Name/Server/Cluster are the display fields (for a
// legacy bare entry Name is the raw key and Server/Cluster are empty).
type CachedCredInfo struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Server  string `json:"server,omitempty"`
	Cluster string `json:"cluster,omitempty"`
	// A zero Expiry (an entry with no expiry) is omitted from the payload
	// rather than rendered as the zero timestamp.
	Expiry time.Time `json:"expiry,omitzero"`
	// TokenRef and TokenUserID name the cached token and its owner when
	// the exec plugin created it in the call that cached it.
	TokenRef    *clierr.TokenRef `json:"tokenRef,omitempty"`
	TokenUserID string           `json:"tokenUserId,omitempty"`
}

// CacheKey scopes an exec-credential cache entry by server identity and
// cluster, so the same --name used against two servers never collides.
// serverHost must be a canonical server identity (CanonicalServerIdentity
// output) so that equivalent URL spellings - including http vs https - produce
// distinct keys. The parameter is named serverHost for historical reasons.
func CacheKey(name, serverHost, cluster string) string {
	return CacheKeyFor(name, serverHost, cluster, "")
}

// CacheKeyFor is the exec plugin's cache key for a call with --name name
// and --user-id userID, for serverHost (a canonical server identity) and
// cluster: CacheKey's form for a call without --user-id, and
// user-id:<userID>/<name>@<serverHost>/<cluster> for one with it. Every
// key the plugin reads or writes is built here. A user id is a Kubernetes
// object name, which holds no "/", so the user id ends at the first "/"
// and two user ids give two keys.
func CacheKeyFor(name, serverHost, cluster, userID string) string {
	key := name + "@" + serverHost + "/" + cluster
	if userID == "" {
		return key
	}
	return UserIDCachePrefix + userID + "/" + key
}

// userIDOfKey is the user id a cache key carries: the one CacheKeyFor puts
// after UserIDCachePrefix, ended by the first "/", or "" for a key of a
// call without --user-id.
func userIDOfKey(key string) string {
	rest, ok := strings.CutPrefix(key, UserIDCachePrefix)
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

// fileStore keeps tokens in a 0600 YAML map file.
type fileStore struct {
	path          string
	cachePath     string
	cacheLockPath string
	mu            sync.Mutex
	// notices is shared with any store wrapping this one (keyringStore's
	// cache is a fileStore instance) so DrainNotices sees every notice
	// recorded by either layer in true record order, from one buffer
	// rather than merging two.
	notices *noticeBuffer
}

func NewFileStore(dir string) CredStore {
	return newFileStore(dir, &noticeBuffer{})
}

// CredentialsFilePath is the plaintext file the file backend writes tokens
// to, named in the disclosure the plugin-created login gives before the
// sign-in's first prompt under that backend.
func CredentialsFilePath(dir string) string { return filepath.Join(dir, "credentials.yaml") }

// newFileStore builds a fileStore recording onto nb - the shared-buffer
// constructor keyringStore uses for its file-backed cache; NewFileStore
// gives a standalone fileStore its own buffer.
func newFileStore(dir string, nb *noticeBuffer) *fileStore {
	return &fileStore{
		path:          CredentialsFilePath(dir),
		cachePath:     filepath.Join(dir, "token-cache.yaml"),
		cacheLockPath: filepath.Join(dir, "token-cache.lock"),
		notices:       nb,
	}
}

// DrainNotices implements runtime.NoticeSource.
func (f *fileStore) DrainNotices() []string { return f.notices.DrainNotices() }

// ResolvedBackend implements CredStore.
func (f *fileStore) ResolvedBackend() (string, bool) { return backendFile, true }

// setTokenHook is a test-only synchronization point, invoked (when non-nil)
// between SetToken's read and write. Lets tests force the exact stale-snapshot
// interleaving of two fileStore instances (two processes' worth of state).
// Nil in production.
//
// The calling test must not use t.Parallel(): the hook is a package-level
// seam shared by every SetToken call.
var setTokenHook func()

// setCachedLock is a test-only synchronization point, invoked (when non-nil)
// while the token-cache.yaml flock is HELD, after the read and before the
// write. Lets tests force an exact interleaving of concurrent SetCachedCred
// calls against different fileStore instances (as kubectl would produce).
// Nil in production.
var setCachedLock func()

// getCachedPruneHook is a test-only synchronization point, invoked (when
// non-nil) inside GetCachedCred's expiry-prune branch, after the lock-free
// read confirms expiry and before withCacheLock is acquired. Lets tests pin
// the exact GetCachedCred-prune vs SetCachedCred interleaving. Nil in
// production.
var getCachedPruneHook func()

// cacheLockWait bounds how long withCacheLock waits for another process to
// release the token-cache lock. A var so tests shorten it.
var cacheLockWait = 30 * time.Second

// SetCacheLockWaitForTest shortens the token-cache lock wait for the
// current test run; call the returned restore in cleanup. Exported for the
// command packages' tests, which assert what a contended cache lock does
// to a delete. Not for production.
func SetCacheLockWaitForTest(d time.Duration) (restore func()) {
	old := cacheLockWait
	cacheLockWait = d
	return func() { cacheLockWait = old }
}

// withCacheLock acquires the cross-process lock for token-cache.yaml, runs fn
// holding it, then releases. The caller must hold f.mu over the entire call so
// that same-process goroutines never contend on the flock; the flock then
// serializes across different fileStore instances (separate processes).
// Plain reads stay lock-free - atomic-rename writes keep them consistent. The
// lock is required for every read-modify-write, which covers SetCachedCred,
// MarkCachedCredReady, DeleteCachedCredsIfUnchanged, DeleteAllCachedCreds,
// the prune branches of GetCachedCred and ListCachedCreds, and the local
// delete's read hold (HoldCachedCreds).
func (f *fileStore) withCacheLock(fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(f.cacheLockPath), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cacheLockWait)
	defer cancel()
	release, err := waitForFlock(ctx, f.cacheLockPath, 50*time.Millisecond)
	if err != nil {
		return clierr.Retryable(clierr.CodeLockTimeout,
			"locking token-cache.yaml (another rancher process may be writing): "+err.Error()).
			WithCause(err)
	}
	defer release()
	return fn()
}

// warnedLoosePerm dedupes the loose-permission warning: read paths run on every
// credential access, so without this the same warning would repeat all run.
var warnedLoosePerm sync.Map // path -> struct{}

// warnLoosePerm reports a non-empty notice when a credential file is
// group/world readable, so a plaintext token in a 0644 file does not sit
// unremarked. It notices at most once per path per process; every
// later call for the same path returns "". On Windows this Unix-perm check
// is SKIPPED via permCheckApplicable (NTFS ignores mode bits; Mode().Perm()
// is synthetic there and would false-positive on every file) - the
// owner-only DACL set by hardenACL at write time is the guarantee on that
// platform. The caller decides where the notice goes: a fileStore method
// records it on its own buffer, while ReadOnlyCachedCred (no store
// receiver, a standalone read on the token shim's fast path) returns it to
// its own caller instead.
func warnLoosePerm(path string) string {
	if fi, err := os.Stat(path); err == nil {
		return warnLoosePermFI(path, fi)
	}
	return ""
}

// warnLoosePermFI is warnLoosePerm's core: checks fi's permissions and
// notices at most once per path. Split out so callers that already hold an
// os.FileInfo (e.g. after os.Open + handle.Stat()) avoid a second syscall.
func warnLoosePermFI(path string, fi os.FileInfo) string {
	if !permCheckApplicable() {
		return ""
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if _, seen := warnedLoosePerm.LoadOrStore(path, struct{}{}); !seen {
			return fmt.Sprintf("%s is group/world-readable (%o); run: chmod 600 %s", path, fi.Mode().Perm(), path)
		}
	}
	return ""
}

func (f *fileStore) read() (map[string]string, error) {
	observeIdentity("credentialsread")
	m := map[string]string{}
	b, err := readFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	f.notices.record(warnLoosePerm(f.path))
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

// SetToken is a lock-free read-modify-write of credentials.yaml: any
// COMMAND that reaches it must run inside config.WithLock (the command
// sites' lock boundary tests are the enforcement point) - a direct call
// from a new command reintroduces the concurrent lost-update this file's
// tests pin.
func (f *fileStore) SetToken(ref ServerRef, token string) error {
	_, err := f.SetTokenWithOutcome(ref, token, PriorCredential{})
	return err
}

// SetTokenWithOutcome is SetToken reporting what the write did. A read or
// decode failure refuses the write outright: without the parsed map the
// write would drop every other server's entry, so the file is left as it
// was and the prior token is unknown. The caller's prior classification is
// ignored: this store's prior is the entry in the file it just read, and
// the file is the only place it can restore.
func (f *fileStore) SetTokenWithOutcome(ref ServerRef, token string, _ PriorCredential) (CommitOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := CommitOutcome{Backend: clierr.BackendFile, State: CommitNotWritten}
	m, err := f.read()
	if err != nil {
		out.PriorSource = clierr.PriorUnknown
		return out, err
	}
	prior, had := m[ref.Name]
	out.PriorSource = clierr.PriorNone
	if had {
		out.PriorSource = clierr.PriorFile
	}
	m[ref.Name] = token
	if setTokenHook != nil {
		setTokenHook()
	}
	if werr := f.write(m); werr != nil {
		// The write renames a fully written temp file into place, so a
		// failure before the rename leaves credentials.yaml unchanged.
		return out, werr
	}
	out.State = CommitCommitted
	if had {
		out.Compensate = func() error { return f.SetToken(ref, prior) }
	} else {
		out.Compensate = func() error { return f.DeleteToken(ref) }
	}
	return out, nil
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
	b, err := readFile(f.cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	f.notices.record(warnLoosePerm(f.cachePath))
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

func (f *fileStore) GetCachedCred(key string, anchor *CacheAnchor) (string, time.Time, error) {
	d, err := f.GetCachedCredDetail(key, anchor)
	return d.Token, d.Expiry, err
}

func (f *fileStore) GetCachedCredDetail(key string, anchor *CacheAnchor) (CachedCredDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Lock-free read: no flock needed for a pure read - atomic-rename writes
	// keep reads consistent.
	m, err := f.readCache()
	if err != nil {
		return CachedCredDetail{}, err
	}
	cred, ok := m[key]
	if !ok {
		return CachedCredDetail{}, ErrNoToken
	}
	live := cred.Expiry.IsZero() || !time.Now().After(cred.Expiry)
	if live && anchor.validFor(key, cred) {
		return CachedCredDetail{Token: cred.Token, Expiry: cred.Expiry, ReadyEndpoints: cred.ReadyEndpoints}, nil
	}
	if live && !anchor.staleRecord(key, cred) {
		// A reader that cannot judge the record serves nothing and
		// deletes nothing: the record may be another process's perfectly
		// good one.
		return CachedCredDetail{}, ErrNoToken
	}
	// Expired, or bound to a login this directory no longer has: prune
	// under the flock. Re-read under the lock so the written
	// snapshot includes any concurrent SetCachedCred writes that landed since
	// the lock-free read above - without the re-read, pruning without the lock
	// is a read-modify-write that clobbers concurrent writes (lost-update).
	// Re-check the entry itself in the re-read (mirroring ListCachedCreds's
	// prune): the key that was expired at the lock-free read may have been
	// refreshed by a concurrent SetCachedCred for the same slot and must not
	// be deleted - serve the fresh credential instead.
	if getCachedPruneHook != nil {
		getCachedPruneHook()
	}
	var refreshed *cachedCred
	if werr := f.withCacheLock(func() error {
		m2, rerr := f.readCache()
		if rerr != nil {
			return rerr
		}
		c, ok := m2[key]
		if !ok {
			return nil
		}
		if (c.Expiry.IsZero() || !time.Now().After(c.Expiry)) && anchor.validFor(key, c) {
			refreshed = &c
			return nil
		}
		delete(m2, key)
		return f.writeCache(m2)
	}); werr != nil {
		return CachedCredDetail{}, werr
	}
	if refreshed != nil {
		return CachedCredDetail{Token: refreshed.Token, Expiry: refreshed.Expiry, ReadyEndpoints: refreshed.ReadyEndpoints}, nil
	}
	return CachedCredDetail{}, ErrNoToken
}

func (f *fileStore) MarkCachedCredReady(key, token, fingerprint string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	marked := false
	err := f.withCacheLock(func() error {
		m, err := f.readCache()
		if err != nil {
			return err
		}
		c, ok := m[key]
		if !ok || c.Token != token || (!c.Expiry.IsZero() && time.Now().After(c.Expiry)) {
			return nil
		}
		if slices.Contains(c.ReadyEndpoints, fingerprint) {
			marked = true
			return nil
		}
		c.ReadyEndpoints = append(c.ReadyEndpoints, fingerprint)
		m[key] = c
		marked = true
		return f.writeCache(m)
	})
	return marked, err
}

func (f *fileStore) SetCachedCred(key, token string, expiry time.Time, name, serverHost, cluster string, binding CacheBinding) error {
	// Refused before the cache lock: the refusal needs nothing but the
	// binding, and a paused entry's writer goes on to try the config lock,
	// which is never taken while this one is held.
	if err := binding.writable(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.withCacheLock(func() error {
		m, err := f.readCache()
		if err != nil {
			return err
		}
		if setCachedLock != nil {
			setCachedLock()
		}
		m[key] = binding.record(token, expiry, name, serverHost, cluster)
		return f.writeCache(m)
	})
}

func (f *fileStore) DeleteAllCachedCreds() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The wipe honors the same cross-process lock as every other
	// token-cache.yaml write: an unlocked Remove can interleave with a
	// concurrent flock-held read-modify-write, whose write-back then
	// silently resurrects the "deleted" entries - the lost-update class the
	// flock exists to close. The lock file is separate from the data file,
	// so removing the data file under the lock is safe.
	return f.withCacheLock(func() error {
		err := removeFile(f.cachePath)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	})
}

func (f *fileStore) ListCachedCreds() ([]CachedCredInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Lock-free read: no flock needed for a pure read - atomic-rename writes
	// keep reads consistent.
	m, err := f.readCache()
	if err != nil {
		return nil, err
	}
	out := make([]CachedCredInfo, 0, len(m))
	var expiredKeys []string
	for key, cred := range m {
		if !cred.Expiry.IsZero() && time.Now().After(cred.Expiry) {
			expiredKeys = append(expiredKeys, key)
			continue
		}
		info := CachedCredInfo{
			Key:         key,
			Name:        cred.Name,
			Server:      cred.Server,
			Cluster:     cred.Cluster,
			Expiry:      cred.Expiry,
			TokenRef:    cred.TokenRef,
			TokenUserID: cred.TokenUserID,
		}
		if info.Name == "" {
			// Legacy bare entry: no record fields - surface the raw key as the name.
			info.Name = key
		}
		out = append(out, info)
	}
	if len(expiredKeys) > 0 {
		// Prune under the flock. Re-read under the lock so the written snapshot
		// includes any entries from concurrent SetCachedCred calls that landed
		// since the lock-free read above. Re-check expiry in the re-read: a key
		// that was expired at scan time may have been refreshed by a concurrent
		// write and must not be deleted. Best-effort: a failed write leaves
		// expired entries on disk for the next pass; never fail the list for it.
		_ = f.withCacheLock(func() error {
			m2, rerr := f.readCache()
			if rerr != nil {
				return rerr
			}
			now := time.Now()
			changed := false
			for _, key := range expiredKeys {
				if c, ok := m2[key]; ok && !c.Expiry.IsZero() && now.After(c.Expiry) {
					delete(m2, key)
					changed = true
				}
			}
			if !changed {
				return nil
			}
			return f.writeCache(m2)
		})
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
// the ephemeral exec-cred cache to a file store - the keychain has no list API,
// and a single file is an enumerable single source of truth.
type keyringStore struct {
	cache CredStore
	// dir is the config directory whose identity records name the keychain
	// accounts this store may read and write. Without it a store could only
	// address an entry by its name, which every directory on the machine
	// spells the same way.
	dir     string
	pinHint string // non-empty when `auto`, not a declaration, chose the keychain
	// notices is the SAME buffer as cache's (both constructed from one
	// newNoticeBuffer() call below) - one buffer, not two merged on drain,
	// so DrainNotices sees a loose-permission notice from the wrapped file
	// cache and a keychain-unreachable notice from this store in true
	// record order.
	notices *noticeBuffer
}

// newPinnedKeyringStore builds a keyring-backed store. When pinned is
// true - `auto` chose the keychain, by the pin or by what an unpinned
// directory has stored - the store carries a remediation hint appended to
// every keyring error, so an unusable keychain fails loudly with a way
// out instead of silently. A policy that names the keychain outright has
// nothing to add.
func newPinnedKeyringStore(dir string, pinned bool) CredStore {
	hint := ""
	if pinned {
		hint = readerKeychainHint()
	}
	nb := &noticeBuffer{}
	return &keyringStore{cache: newFileStore(dir, nb), dir: dir, pinHint: hint, notices: nb}
}

// DrainNotices implements runtime.NoticeSource.
func (k *keyringStore) DrainNotices() []string { return k.notices.DrainNotices() }

// ResolvedBackend implements CredStore.
func (k *keyringStore) ResolvedBackend() (string, bool) { return backendKeychain, true }

// keyringUnreachable reports whether err is a backend-unreachable keyring
// failure - a transport/connection-class error (Linux: godbus "no session
// bus" on headless; macOS: locked/unavailable Keychain; Windows:
// Credential Manager transport) - as opposed to a clean miss
// (keyring.ErrNotFound, which is NOT unreachable) or an oversized-value
// failure (keyring.ErrSetDataTooBig - wincred rejects a Set over 2560
// bytes, macOS's security CLI over 4096 - a data-size problem, not a
// connectivity one). go-keyring exposes no typed transport errors, so any
// other non-miss failure is treated as unreachable. keychainError reports
// such a failure as CREDENTIAL_STORE_UNAVAILABLE.
func keyringUnreachable(err error) bool {
	return err != nil && !errors.Is(err, keyring.ErrNotFound) && !errors.Is(err, keyring.ErrSetDataTooBig)
}

// keyringGet reads one account, counting the operation.
func keyringGet(account string) (string, error) {
	if keyringObserverFn != nil {
		keyringObserverFn("get")
	}
	return keyringBackend.Get(keyringService, account)
}

// keyringSet writes one account, counting the operation.
func keyringSet(account, token string) error {
	if keyringObserverFn != nil {
		keyringObserverFn("set")
	}
	if err := keyringBackend.Set(keyringService, account, token); err != nil {
		return err
	}
	afterWrite("keychain set")
	return nil
}

// keyringDelete removes one account, counting the operation.
func keyringDelete(account string) error {
	if keyringObserverFn != nil {
		keyringObserverFn("delete")
	}
	if err := keyringBackend.Delete(keyringService, account); err != nil {
		return err
	}
	afterWrite("keychain delete")
	return nil
}

// credentialReplacedError says the keychain slot holds a credential this
// directory never stored: another config directory that shares this one's
// identity wrote it, or a login is finishing right now.
func credentialReplacedError(entry string) error {
	return clierr.New(clierr.CodeCredentialReplaced, fmt.Sprintf(
		"the stored credential for %s was replaced by another config directory that shares this one's identity, or by a login in progress; retry, then log in again",
		clierr.ShellQuote(entry))).
		WithHint(LoginHintFor(entry)).
		WithCause(ErrCredentialIdentity)
}

// loginIncompleteError says a login for the entry wrote its identity ahead
// of the credential and has not settled it: either it is still running, or
// it ended before it could.
func loginIncompleteError(entry string) error {
	return clierr.Retryable(clierr.CodeLoginIncomplete, fmt.Sprintf(
		"a login for %s is in progress or did not complete; retry, then log in again",
		clierr.ShellQuote(entry))).
		WithHint(LoginHintFor(entry)).
		WithCause(ErrCredentialIdentity)
}

// getError renders a failed keychain read. A declared keychain that cannot
// be reached is an error with the way out, never a quiet read of some other
// store: a second source is exactly how a directory ends up serving a
// credential it did not store.
func (k *keyringStore) getError(entry string, err error) error {
	return keychainError("get", entry, err, k.pinHint)
}

// GetToken reads the entry's identity record first: with no record nothing
// was ever stored for the entry under this directory's identity, so there
// is no account to read and no keychain call to make. With a record, the
// account it names is read once and the credential's keyed fingerprint
// decides whether this directory may use it.
func (k *keyringStore) GetToken(ref ServerRef) (string, error) {
	rec, err := readIdentityRecord(k.dir, ref.Name)
	if err != nil {
		return "", err
	}
	if rec == nil {
		return "", ErrNoToken
	}
	if rec.State == identityPending {
		return "", loginIncompleteError(ref.Name)
	}
	tok, err := keyringGet(rec.Account())
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return "", ErrNoToken
	case err != nil:
		return "", k.getError(ref.Name, err)
	case credentialFingerprint(rec.ID, tok) != rec.Fingerprint:
		return "", credentialReplacedError(ref.Name)
	}
	return tok, nil
}

// SetToken writes a credential with the full identity transaction around
// it. Production logins reach the keychain through CommitToken, which runs
// that transaction itself with the command's context and its own
// classification of the prior; this path exists for a caller that has only
// the store interface, and takes the identity lock with a background
// context, still bounded by the lock's own deadline.
func (k *keyringStore) SetToken(ref ServerRef, token string) error {
	write, err := beginKeychainWrite(context.Background(), k, ref, token)
	if err != nil {
		return err
	}
	out, err := k.SetTokenWithOutcome(ref, token, write.Prior)
	// A write that committed and then reported an error carries its
	// advisory here instead of in a return value this signature has no
	// room for; the invocation's drain surfaces it. Recorded before the
	// record is settled, so a failure to settle cannot swallow the one
	// line saying the credential did land.
	k.notices.record(out.Notice)
	if ferr := write.finish(context.Background(), out.State); ferr != nil {
		return errors.Join(err, ferr)
	}
	return err
}

// SetTokenWithOutcome writes the token to the account the caller's prior
// names and normalizes what the keychain did. It reads no prior itself: the
// caller classified one, and a second read here could disagree with the
// identity record the login already wrote. A keyring Set can commit and
// still report an error, so a failed Set is followed by exactly one
// read-back: the value that came back is what decides between not written,
// written, and unknown.
func (k *keyringStore) SetTokenWithOutcome(ref ServerRef, token string, prior PriorCredential) (CommitOutcome, error) {
	out := CommitOutcome{Backend: clierr.BackendKeyring, State: CommitNotWritten, PriorSource: prior.Source}
	if prior.Account == "" {
		return out, fmt.Errorf("keyring set %q: no credential identity for the entry", ref.Name)
	}
	hasKeyringPrior := prior.Source == clierr.PriorKeyring
	// Compensation restores THIS backend's prior physical state: the
	// account's own prior value, or the removal of an item that was not
	// there before. A file entry belongs to the file store and is never
	// touched here, so a keychain undo can never erase it.
	compensate := func() error {
		if hasKeyringPrior {
			return keyringSet(prior.Account, prior.Raw)
		}
		return keyringDelete(prior.Account)
	}
	commit := func() CommitOutcome {
		if prior.Source == clierr.PriorUnknown {
			// No prior snapshot: the write landed and cannot be undone.
			out.State = CommitCommittedUncompensatable
			return out
		}
		out.State = CommitCommitted
		out.Compensate = compensate
		return out
	}

	err := keyringSet(prior.Account, token)
	if err == nil {
		return commit(), nil
	}
	back, berr := keyringGet(prior.Account)
	switch {
	case berr == nil && back == token:
		out = commit()
		out.Notice = "keyring reported an error after storing the token: " + err.Error()
		return out, nil
	case berr == nil && hasKeyringPrior && back == prior.Raw,
		errors.Is(berr, keyring.ErrNotFound) && !hasKeyringPrior && prior.Source != clierr.PriorUnknown:
		// The keychain holds exactly what it held before the write.
		return out, k.setError(ref, err)
	default:
		out.State = CommitPossiblyCommitted
		return out, k.setError(ref, err)
	}
}

// setError renders a failed keychain write. NEVER silently
// degrade a login write to the plaintext file - the pin's guarantee is that
// a transient keychain failure cannot downgrade the machine's storage.
// Refuse with the keychain's own remedy instead.
func (k *keyringStore) setError(ref ServerRef, err error) error {
	return keychainError("set", ref.Name, err, k.pinHint)
}

// DeleteToken removes the entry's credential from the keychain and then its
// identity record. It touches the file store never: the two stores hold
// separate credentials, and on a directory whose store is moving, deleting
// the file entry here would remove the credential a login just committed.
// The identity lock is taken with a background context for the reason
// SetToken documents.
func (k *keyringStore) DeleteToken(ref ServerRef) error {
	_, notices, err := deleteKeychainCredential(context.Background(), k.dir, ref, true, k.pinHint)
	for _, n := range notices {
		k.notices.record(n)
	}
	return err
}

func (k *keyringStore) GetCachedCred(key string, anchor *CacheAnchor) (string, time.Time, error) {
	return k.cache.GetCachedCred(key, anchor)
}
func (k *keyringStore) GetCachedCredDetail(key string, anchor *CacheAnchor) (CachedCredDetail, error) {
	return k.cache.GetCachedCredDetail(key, anchor)
}
func (k *keyringStore) SetCachedCred(key, token string, expiry time.Time, name, serverHost, cluster string, binding CacheBinding) error {
	return k.cache.SetCachedCred(key, token, expiry, name, serverHost, cluster, binding)
}
func (k *keyringStore) MarkCachedCredReady(key, token, fingerprint string) (bool, error) {
	return k.cache.MarkCachedCredReady(key, token, fingerprint)
}
func (k *keyringStore) DeleteAllCachedCreds() error                { return k.cache.DeleteAllCachedCreds() }
func (k *keyringStore) ListCachedCreds() ([]CachedCredInfo, error) { return k.cache.ListCachedCreds() }

// DeleteLoginToken deletes a server's login token from the keychain, file
// store, and every helper the records file says was asked to store a
// credential for the server. The exec union covers: the current policy's
// helper (with the current identity) and every older record - so a policy
// change between login and logout never strands a helper credential. It is
// the login delete of auth logout, config delete-server and config
// update-server, through DeleteAndRecord in internal/token. The keychain and
// file store are deleted unconditionally and independently of policy: the two
// stores hold separate credentials and a directory's store may have changed
// between the login and the logout. No backend's failure short-circuits
// another; per-backend errors are joined so a live token is never left behind
// unreported.
func DeleteLoginToken(ctx context.Context, dir string, ref ServerRef, policy string) (from, notices []string, err error) {
	var errs []error
	// The keychain step runs when this directory has an identity record for
	// the entry: the record is the only thing that names the account, so
	// without one nothing was ever stored there under this directory's
	// identity and there is nothing to reach. Whether the directory keeps
	// its credentials in the keychain decides only how a keychain it cannot
	// reach is reported: loudly for the store the directory uses, as a
	// warning that keeps the record for one it merely has a leftover record
	// in.
	usesKeychain, hint, perr := keychainIsTheStore(dir, policy)
	if perr != nil {
		return nil, nil, perr
	}
	deletedFromKeychain, keychainNotices, kerr := deleteKeychainCredential(ctx, dir, ref, usesKeychain, hint)
	notices = append(notices, keychainNotices...)
	if kerr != nil {
		errs = append(errs, kerr)
	}
	if deletedFromKeychain {
		from = append(from, "keychain")
	}
	fs := newFileStore(dir, &noticeBuffer{})
	switch _, terr := fs.GetToken(ref); {
	case terr == nil:
		if derr := fs.DeleteToken(ref); derr != nil {
			errs = append(errs, fmt.Errorf("file credential store: %w", derr))
		} else {
			from = append(from, "file")
		}
	case errors.Is(terr, ErrNoToken):
		// nothing stored in the file backend - fine
	default:
		// NEVER report a successful delete when the file store is
		// unreadable/corrupt: a live plaintext token may remain.
		errs = append(errs, fmt.Errorf("file credential store: %w", terr))
	}

	// Exec helpers: erase through the union of the current policy's helper
	// (with the current identity) and every recorded entry for this server -
	// a policy change between login and logout must not strand a helper
	// credential. The records lock spans erase + unrecord.
	// One deduplicated target list, in a stable order: the policy's helper
	// (when any) first, then the recorded entries as stored. Dedup is
	// helperRecords.add - the same append-if-absent every record write
	// uses - on a throwaway in-memory list (its dirty flag is never read
	// here), so the package has exactly one dedup implementation.
	var targets helperRecords
	if name, ok := execHelperName(policy); ok {
		if !credhelper.ValidHelperName(name) {
			errs = append(errs, fmt.Errorf("invalid credential helper name %q in effective policy; refusing to execute it", name))
		} else if cu, cerr := credhelper.CanonicalServerURL(ref.URL); cerr == nil {
			targets.add(HelperRecord{Helper: name, Name: ref.Name, CanonicalURL: cu})
		} else if ref.URL != "" {
			errs = append(errs, fmt.Errorf("credential helper delete: %w", cerr))
		}
	}
	lerr := withHelperRecordsLock(ctx, dir, func(st *helperRecords) error {
		for _, e := range st.forServer(ref.Name) {
			targets.add(e)
		}
		return deleteHelperRecords(dir, targets.Entries, st, func(e HelperRecord) {
			from = append(from, "helper:"+e.Helper)
		})
	})
	if lerr != nil {
		errs = append(errs, lerr)
	}
	// fs is a throwaway *fileStore never exposed to any caller as a
	// NoticeSource - its own buffer (a loose-permission notice on
	// credentials.yaml, say) would otherwise be drained by no one.
	notices = append(notices, fs.DrainNotices()...)
	return from, notices, errors.Join(errs...)
}

// keychainIsTheStore reports whether this directory keeps its credentials
// in the keychain, and the hint its keychain errors carry. Under `auto`
// the pin answers, and without a pin what the directory has stored does:
// any identity record means the keychain, whether or not file tokens sit
// beside it, and the both-stores error is never raised here - a logout is
// its remedy and has to be able to run. The config is loaded only in that
// last case, for the entries it names.
func keychainIsTheStore(dir, policy string) (bool, string, error) {
	p, err := EffectivePolicy(policy)
	if err != nil {
		return false, "", err
	}
	switch p {
	case backendKeychain:
		return true, "", nil
	case "auto":
	default:
		return false, "", nil
	}
	switch ReadPin(dir) {
	case backendKeychain:
		return true, readerKeychainHint(), nil
	case backendFile:
		return false, "", nil
	}
	cfg, err := Load(FilePath(dir))
	if err != nil {
		return false, "", err
	}
	u, err := UnpinnedStoreOf(dir, cfg)
	if err != nil || !u.UsesKeychain() {
		return false, "", err
	}
	return true, readerKeychainHint(), nil
}

// deleteHelperRecords erases every record through eraseHelperCredential:
// a successfully erased record is removed from st (a failed one stays for
// retry, its error joined), and deleted is called for each record whose
// credential actually existed. The one loop behind DeleteLoginToken's
// per-server delete and DeleteOrphanedHelperCredentials' sweep, so a fix to
// either path cannot land on only one copy.
func deleteHelperRecords(dir string, records []HelperRecord, st *helperRecords, deleted func(HelperRecord)) error {
	var errs []error
	for _, e := range records {
		wasDeleted, err := eraseHelperCredential(dir, e)
		if err != nil {
			errs = append(errs, fmt.Errorf("credential helper %q erase: %w", e.Helper, err))
			continue // record stays for retry
		}
		st.remove(e)
		if wasDeleted {
			deleted(e)
		}
	}
	return errors.Join(errs...)
}

// eraseHelperCredential asks e.Helper to erase the recorded credential and
// reports whether a stored credential was actually deleted. Used by the
// delete paths, which span records for potentially different helpers and so
// construct a store per record; a caller that already holds the right
// execStore (DeleteToken) uses eraseCredential on it directly.
func eraseHelperCredential(dir string, e HelperRecord) (deleted bool, err error) {
	return newExecStore(e.Helper, dir).eraseCredential(credhelper.Request{ServerURL: e.CanonicalURL, Name: e.Name})
}

// CanonicalServerIdentity derives the full canonical cache identity from a
// server URL: the canonical URL output of credhelper.CanonicalServerURL
// (scheme://host with the scheme lowercased and the default port for that
// scheme stripped). Falls back to a lowercased raw string on malformed input.
// Use at every exec-credential cache-identity generation site so that
// http://host and https://host produce distinct keys; use ServerHost for
// non-identity needs (e.g. kubeconfig entry names).
func CanonicalServerIdentity(rawURL string) string {
	if cu, err := credhelper.CanonicalServerURL(rawURL); err == nil && cu != "" {
		return cu
	}
	return strings.ToLower(rawURL)
}

// canonicalServerHost derives the canonical cache-identity host from a full
// server URL: the host lowercased and stripped of its scheme's default port
// (443 for https, 80 for http). Falls back to a lowercased ServerHost when
// the URL cannot be canonicalized, so callers always get a non-empty string
// for a non-empty URL. Used by CacheRecordForServer for legacy record
// normalization; use ServerHost for non-cache identity (e.g. kubeconfig
// entry names).
func canonicalServerHost(rawURL string) string {
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
// CanonicalServerIdentity so that equivalent URL spellings produce the same
// key and http/https are kept distinct.
func ServerHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// OpenStore selects a store the way a reader of the directory does.
// policy is the config-file-level declaration;
// RANCHER_CLI_CREDENTIAL_STORE, when set, outranks it - same precedence as
// everywhere else env beats config - so an explicit "keychain" argument
// here can still resolve to the file backend under that env var. Explicit
// policies ("keychain"/"file") are honored directly and never consult the
// pin. "auto" reads the pin, and with none, what the directory's entries
// have stored (UnpinnedStoreOf), for which it loads config.yaml. Nothing
// is probed and nothing is written. Unknown policies are hard errors.
// A command that already holds its config uses OpenStoreFor.
func OpenStore(policy, dir string) (CredStore, error) {
	resolved, err := EffectivePolicy(policy)
	if err != nil {
		return nil, err
	}
	return openStoreResolved(resolved, dir, func() (*Config, error) { return Load(FilePath(dir)) })
}

// OpenStoreFor is OpenStore for a caller that has loaded the directory's
// config already: the policy is its declaration, and the entries an
// unpinned `auto` directory is judged by are its entries.
func OpenStoreFor(cfg *Config, dir string) (CredStore, error) {
	resolved, err := EffectivePolicy(cfg.CredentialStore)
	if err != nil {
		return nil, err
	}
	return openStoreResolved(resolved, dir, func() (*Config, error) { return cfg, nil })
}

// OpenDeclaredStore opens the store cfg's declaration names, whatever
// RANCHER_CLI_CREDENTIAL_STORE says: the store an exec entry that sets the
// variable to the empty value reads. cfg must declare one.
func OpenDeclaredStore(cfg *Config, dir string) (CredStore, error) {
	if cfg.CredentialStore == "" {
		return nil, errors.New("the config directory declares no credential store")
	}
	return openStoreResolved(cfg.CredentialStore, dir, func() (*Config, error) { return cfg, nil })
}

// execHelperName extracts the helper name from an "exec:<name>" policy value.
func execHelperName(policy string) (string, bool) {
	name, ok := strings.CutPrefix(policy, "exec:")
	if !ok {
		return "", false
	}
	return name, true
}

// CheckStorePolicy reports whether p is a credential-store value this
// release understands. It is the one allowlist - adding a backend means
// updating it and nothing else - and a login validates the store it is
// asked for through it rather than carrying a second copy.
func CheckStorePolicy(p string) error { return checkPolicy(p) }

// OpenResolvedStore opens the store for an ALREADY-PHYSICAL backend:
// file, keychain or exec:<name>. It applies neither the variable nor the
// pin, and writes no pin, which is what the login path needs - the login
// resolves the backend itself and the pin it ends with is written by its
// own last step, after the credential commits. The notice it returns is
// always empty; the login's decision owns every disclosure.
func OpenResolvedStore(backend, dir string) (CredStore, string, error) {
	store, err := openStoreResolved(backend, dir, func() (*Config, error) { return &Config{}, nil })
	return store, "", err
}

// checkPolicy returns the error for an unknown effective policy value p.
// Both openStoreResolved and NewLazyStore call this so the valid-policy
// allowlist is defined exactly once - adding a new backend means updating
// this one allowlist - both paths share it, so they cannot silently diverge.
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
//
// It never probes the keychain and never writes the pin: only a login
// decides where a directory keeps its credentials. An `auto` directory
// with no pin is answered from what its entries have stored, and entries
// is called only then. Nothing stored opens the file store, whose read
// answers "not logged in"; credentials in both stores are an error.
func openStoreResolved(p, dir string, entries func() (*Config, error)) (CredStore, error) {
	backend, byAuto, err := resolveBackend(p, dir, entries)
	if err != nil {
		return nil, err
	}
	if name, ok := execHelperName(backend); ok {
		return newExecStore(name, dir), nil
	}
	if backend == backendKeychain {
		return newPinnedKeyringStore(dir, byAuto), nil
	}
	return NewFileStore(dir), nil
}

// resolveBackend is the store a reader of the directory resolves for an
// ALREADY-EFFECTIVE policy, named rather than opened: the policy itself
// when it names a store, else the pin, else what the entries have stored.
// byAuto says `auto` chose it rather than a declaration. It reads the pin
// and, with none, the unpinned decision's files, and nothing else.
func resolveBackend(p, dir string, entries func() (*Config, error)) (backend string, byAuto bool, err error) {
	if err := checkPolicy(p); err != nil {
		return "", false, err
	}
	if _, ok := execHelperName(p); ok || p == backendFile || p == backendKeychain {
		return p, false, nil
	}
	switch pin := ReadPin(dir); pin {
	case backendKeychain, backendFile:
		return pin, true, nil
	}
	cfg, err := entries()
	if err != nil {
		return "", false, err
	}
	u, err := UnpinnedStoreOf(dir, cfg)
	if err != nil {
		return "", false, err
	}
	switch u.Row {
	case UnpinnedKeychain:
		return backendKeychain, true, nil
	case UnpinnedBoth:
		return "", false, u.Err()
	}
	return backendFile, true, nil
}

// readerBackend is the store a reader of cfg resolves now in this
// process: cfg's declaration and the variable as every command applies
// them, then the pin or the unpinned decision read now. The conflict of
// a declaration and the variable is its error, as it is every reader's.
func readerBackend(dir string, cfg *Config) (string, error) {
	p, err := EffectivePolicy(cfg.CredentialStore)
	if err != nil {
		return "", err
	}
	backend, _, err := resolveBackend(p, dir, func() (*Config, error) { return cfg, nil })
	return backend, err
}

// keyringProbeTimeout bounds keyringUsable: on macOS the underlying `security`
// exec can block indefinitely without a usable keychain session (headless/SSH/
// sandbox), and a hung probe must degrade to the file store, not hang login.
// Package var so tests can shrink it.
var keyringProbeTimeout = 5 * time.Second

// keyringProbe does the harmless set/delete of a marker key and returns the
// set's error, nil when the keychain took the write. Package var so tests
// can stub it with a never-returning body to exercise the timeout.
var keyringProbe = func() error {
	const probe = "__rancher_cli_probe__"
	if err := keyringBackend.Set(keyringService, probe, "1"); err != nil {
		return err
	}
	_ = keyringBackend.Delete(keyringService, probe)
	return nil
}

// errKeyringProbeTimedOut is keyringUsable's answer for a probe that did
// not return within keyringProbeTimeout: the keychain is unusable, as it
// is for any failure that is not a locked keychain.
var errKeyringProbeTimedOut = errors.New("the keychain did not answer within the probe's time limit")

// keyringUsable runs keyringProbe bounded to keyringProbeTimeout and returns
// nil when the keychain is usable, the probe's own error when it failed, or
// errKeyringProbeTimedOut. The probe goroutine may outlive the timeout (the
// exec is not cancellable) - acceptable for a once-per-login probe.
// probe/timeout are snapshotted before the goroutine starts so a test
// restoring the package vars right after the timeout fires never races with
// the leaked goroutine's read of them. Package var so tests can count/stub
// probes.
var keyringUsable = func() error {
	probe, timeout := keyringProbe, keyringProbeTimeout
	done := make(chan error, 1)
	go func() { done <- probe() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return errKeyringProbeTimedOut
	}
}

var keyringUsableOnceFn = sync.OnceValue(func() error { return keyringUsable() })

// keyringUsableOnce runs keyringUsable at most once per process and caches
// the result. A CLI invocation is one process: a login's check before its
// sign-in and the same login's backend resolution under the lock would
// otherwise pay the (bounded, keyringProbeTimeout-worst-case) cost twice
// sequentially on an unusable keychain. keyringUsable itself stays the
// swappable seam for tests that exercise it directly (the timeout test);
// every other caller observes the memoized result.
func keyringUsableOnce() error {
	return keyringUsableOnceFn()
}

// resetKeyringUsableCache clears keyringUsableOnce's memoized result.
// Tests that stub keyringUsable and exercise the probe through a store
// open or a login's own backend resolution must call this first, so their
// own stub is actually invoked rather than a cached result left over from
// an earlier test in the same process.
func resetKeyringUsableCache() {
	keyringUsableOnceFn = sync.OnceValue(func() error { return keyringUsable() })
}

// ResetKeyringUsableCacheForTest resets the memoized keyring-usability probe
// so the next call to keyringUsableOnce re-runs the probe. It registers a
// t.Cleanup that resets the cache again at test teardown, so later tests in
// the same process get a fresh probe too. Call this from any package whose
// tests need a clean probe - the unexported resetKeyringUsableCache is not
// reachable outside this package.
//
// The calling test MUST NOT use t.Parallel(): keyringUsable and the cache are
// process-global variables, and a parallel reset races with any other test
// that probes the keychain.
func ResetKeyringUsableCacheForTest(t interface {
	Helper()
	Cleanup(func())
}) {
	t.Helper()
	resetKeyringUsableCache()
	t.Cleanup(resetKeyringUsableCache)
}
