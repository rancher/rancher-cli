package config

import (
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// CacheRecordKind names the rule a cache record was written by, which
// says whose credential made it: the identified entry's own, another
// answer of a call that identified an entry, or a call that identified
// none. A record written in another shape has none.
type CacheRecordKind string

const (
	CacheRecordOwnCredential CacheRecordKind = recordOwnCredential
	CacheRecordIdentified    CacheRecordKind = recordIdentified
	CacheRecordServer        CacheRecordKind = recordServer
	CacheRecordOwner         CacheRecordKind = recordOwner
)

// CachedRecord is one exec-credential cache record as a delete that
// revokes its token reads it: the token, what the record names about it,
// and the entries its binding names.
type CachedRecord struct {
	Key     string
	Token   string
	Expiry  time.Time
	Name    string
	Server  string
	Cluster string
	// TokenRef and TokenUserID name the token and its owner when the exec
	// plugin created it in the call that cached it.
	TokenRef    *clierr.TokenRef
	TokenUserID string
	Kind        CacheRecordKind
	// Identified is the entry the call's --name identified, empty for
	// none.
	Identified string
	// Entries is the list the record is bound to, in the order it was
	// written; Listed reports whether the record has one (a list may be
	// present and empty).
	Entries []string
	Listed  bool

	// bound is the record as stored, which StaleAfterURLChange judges.
	bound cachedCred
}

func cachedRecordOf(key string, c cachedCred) CachedRecord {
	r := CachedRecord{Key: key, Token: c.Token, Expiry: c.Expiry, Name: c.Name, Server: c.Server, Cluster: c.Cluster,
		TokenUserID: c.TokenUserID, Identified: c.Identified, bound: c}
	if c.TokenRef != nil {
		ref := *c.TokenRef
		r.TokenRef = &ref
	}
	if c.Version == cacheRecordVersion {
		r.Kind = CacheRecordKind(c.Kind)
	}
	if c.Entries != nil {
		r.Listed = true
		for _, e := range *c.Entries {
			r.Entries = append(r.Entries, e.Name)
		}
	}
	return r
}

// NamesEntry reports whether the record's binding names entry: as the
// entry its call identified, or in its list.
func (r CachedRecord) NamesEntry(entry string) bool {
	return r.Identified == entry || slices.Contains(r.Entries, entry)
}

// CacheReadError is a cache file that could not be read or parsed: what a
// hold returns, before its callback runs, when the lock was taken and the
// file was not usable. Any other hold failure - the lock not taken above
// all - is not one.
type CacheReadError struct{ Err error }

func (e *CacheReadError) Error() string { return "reading token-cache.yaml: " + e.Err.Error() }
func (e *CacheReadError) Unwrap() error { return e.Err }

// HoldCachedCreds runs fn under the token-cache lock with every record the
// cache holds, deleting nothing. fn runs with the store's mutex held, so it
// calls no store method; a config write is allowed, since the config lock,
// which a caller holds already, is the outer one. A cache file that cannot
// be read is a *CacheReadError, before fn runs.
func (f *fileStore) HoldCachedCreds(fn func([]CachedRecord) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.withCacheLock(func() error {
		m, err := f.readCache()
		if err != nil {
			return &CacheReadError{Err: err}
		}
		recs := make([]CachedRecord, 0, len(m))
		for _, key := range slices.Sorted(maps.Keys(m)) {
			recs = append(recs, cachedRecordOf(key, m[key]))
		}
		return fn(recs)
	})
}

// DeleteCachedCredsIfUnchanged deletes, under the token-cache lock, each
// record of want whose token is still want's, and reports the keys it
// deleted and the keys whose record now holds another token, which it
// leaves in place. A key with no record is in neither list: a reader
// already removed it.
func (f *fileStore) DeleteCachedCredsIfUnchanged(want map[string]string) (deleted, changed []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	err = f.withCacheLock(func() error {
		m, rerr := f.readCache()
		if rerr != nil {
			return rerr
		}
		for _, key := range slices.Sorted(maps.Keys(want)) {
			c, ok := m[key]
			switch {
			case !ok:
			case c.Token != want[key]:
				changed = append(changed, key)
			default:
				delete(m, key)
				deleted = append(deleted, key)
			}
		}
		if len(deleted) == 0 {
			return nil
		}
		return f.writeCache(m)
	})
	if err != nil {
		return nil, nil, err
	}
	return deleted, changed, nil
}

func (k *keyringStore) HoldCachedCreds(fn func([]CachedRecord) error) error {
	return k.cache.HoldCachedCreds(fn)
}

func (k *keyringStore) DeleteCachedCredsIfUnchanged(want map[string]string) (deleted, changed []string, err error) {
	return k.cache.DeleteCachedCredsIfUnchanged(want)
}

func (s *execStore) HoldCachedCreds(fn func([]CachedRecord) error) error {
	return s.cache.HoldCachedCreds(fn)
}

func (s *execStore) DeleteCachedCredsIfUnchanged(want map[string]string) (deleted, changed []string, err error) {
	return s.cache.DeleteCachedCredsIfUnchanged(want)
}

func (l *lazyStore) HoldCachedCreds(fn func([]CachedRecord) error) error {
	s, err := l.backend()
	if err != nil {
		return err
	}
	return s.HoldCachedCreds(fn)
}

func (l *lazyStore) DeleteCachedCredsIfUnchanged(want map[string]string) (deleted, changed []string, err error) {
	s, err := l.backend()
	if err != nil {
		return nil, nil, err
	}
	return s.DeleteCachedCredsIfUnchanged(want)
}

// CacheRecordForServer reports whether a record's server is serverURL's
// canonical identity, the match a server's cache delete uses: a
// record carrying a full URL compares byte for byte with the identity, so
// http and https differ; a record carrying only a host (written before
// records carried a scheme) compares by canonical host under serverURL's
// scheme. A record with no server matches no server.
func CacheRecordForServer(recordServer, serverURL string) bool {
	identity := CanonicalServerIdentity(serverURL)
	if identity == "" || recordServer == "" {
		return false
	}
	if strings.Contains(recordServer, "://") {
		return recordServer == identity
	}
	scheme := "https"
	if u, err := url.Parse(identity); err == nil && u.Scheme != "" {
		scheme = strings.ToLower(u.Scheme)
	}
	return canonicalServerHost(scheme+"://"+recordServer) == canonicalServerHost(identity)
}

// ServerAfterUpdate is the entry config update-server writes for e: with the
// credentials cleared (the URL changes to another server) only the address,
// the trust, the cache binding id, the pause and the superseded tokens
// stay; with other fields changed (a CA) the URL and the CA are set and the
// CA fingerprint is cleared; otherwise only the URL changes (the user
// information left it). It is the one function the write and the judgement
// of the cache records it makes stale share.
func ServerAfterUpdate(e Server, canonicalURL, ca string, credentialsCleared, fieldsChanged bool) Server {
	switch {
	case credentialsCleared:
		return Server{URL: canonicalURL, CA: ca,
			CacheBindingID: e.CacheBindingID, CacheWritesPaused: e.CacheWritesPaused, SupersededTokens: e.SupersededTokens}
	case fieldsChanged:
		e.URL, e.CA, e.CAFingerprint, e.CAFromEnvironment = canonicalURL, ca, "", false
	default:
		e.URL = canonicalURL
	}
	return e
}

// StaleAfterURLChange reports whether the exec plugin's lookup serves rec over c
// and stops serving it once entry is after, the entry config update-server
// writes (ServerAfterUpdate): a record bound to the usable entries at its
// server, when the change makes the entry usable there, or one that names the
// entry. Nothing in c changes.
func (c *Config) StaleAfterURLChange(rec CachedRecord, entry string, after Server) bool {
	if _, ok := c.Servers[entry]; !ok || rec.bound.Version != cacheRecordVersion || !c.servesRecord(rec.bound) {
		return false
	}
	changed := *c
	changed.Servers = maps.Clone(c.Servers)
	changed.Servers[entry] = after
	return !changed.servesRecord(rec.bound)
}

// KeptCachedCred is a record KeepStaleCachedCreds moved: its new key, the
// token it holds, the kind of record and the server of the call that cached
// it.
type KeptCachedCred struct {
	Key    string
	Ref    clierr.TokenRef
	Kind   CacheRecordKind
	Server string
}

// KeepStaleCachedCreds moves, in one write under the token-cache lock, each
// live record with a token reference that the update of entry to after makes
// the lookup stop serving (StaleAfterURLChange over cfg, the config as it is
// before the update) to its kept key, where no read deletes it and the revoke
// plan still finds it. A record with no token reference, an expired one and
// one the update keeps serving stay as they are. It sends nothing.
func KeepStaleCachedCreds(dir string, cfg *Config, entry string, after Server) ([]KeptCachedCred, error) {
	f := newFileStore(dir, &noticeBuffer{})
	var kept []KeptCachedCred
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.withCacheLock(func() error {
		m, err := f.readCache()
		if err != nil {
			return &CacheReadError{Err: err}
		}
		for _, key := range slices.Sorted(maps.Keys(m)) {
			c := m[key]
			if c.TokenRef == nil || (!c.Expiry.IsZero() && time.Now().After(c.Expiry)) ||
				!cfg.StaleAfterURLChange(cachedRecordOf(key, c), entry, after) {
				continue
			}
			delete(m, key)
			kept = append(kept, KeptCachedCred{Key: keepCachedRecord(m, c), Ref: *c.TokenRef, Kind: CacheRecordKind(c.Kind), Server: c.Server})
		}
		if len(kept) == 0 {
			return nil
		}
		return f.writeCache(m)
	})
	if err != nil {
		return nil, err
	}
	return kept, nil
}

// InDeleteScope reports whether deleting entry's credential deletes rec:
// a record for entry's server whose binding names entry (as the entry its
// call identified, or in its list, under any cache binding id, so a record
// a racing writer left with an older id is included), or one whose binding
// names no entry c still has. Every such record is one the entry's binding
// advance turns into a miss, or one no entry can read; a record another
// entry still reads stays. c is the config before the change.
func (c *Config) InDeleteScope(rec CachedRecord, entry string) bool {
	s, ok := c.Servers[entry]
	if !ok || !CacheRecordForServer(rec.Server, s.URL) {
		return false
	}
	if rec.NamesEntry(entry) {
		return true
	}
	if rec.Identified != "" {
		if _, ok := c.Servers[rec.Identified]; ok {
			return false
		}
	}
	return !slices.ContainsFunc(rec.Entries, func(n string) bool { _, ok := c.Servers[n]; return ok })
}
