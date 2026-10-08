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
}

func cachedRecordOf(key string, c cachedCred) CachedRecord {
	r := CachedRecord{Key: key, Token: c.Token, Expiry: c.Expiry, Name: c.Name, Server: c.Server, Cluster: c.Cluster,
		TokenUserID: c.TokenUserID, Identified: c.Identified}
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
