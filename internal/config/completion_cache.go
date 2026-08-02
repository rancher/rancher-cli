package config

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"sigs.k8s.io/yaml"
)

// ReadOnlyCachedCreds returns all unexpired exec-credential cache entries
// without pruning expired ones from disk (AC-6a, AC-27). The existing
// ListCachedCreds prunes on read; this function is the strictly-read-only
// counterpart used on every completion path.
func ReadOnlyCachedCreds(dir string) ([]CachedCredInfo, error) {
	return readCacheROFromFile(filepath.Join(dir, "token-cache.yaml"))
}

// loadCacheMap reads and unmarshals token-cache.yaml at path. A missing
// file is an empty cache, not an error. Never writes to disk. Deliberately
// SILENT on loose permissions: completion paths read through here, and any
// stderr output would appear as a TAB candidate (the same rationale as
// ReadFileTokenSilent) — the shim's fast-path reader warns instead. The
// remaining readCache parity is intentional: no in-process mu (read-only),
// no expiry pruning (documented on ReadOnlyCachedCred), no cross-process
// flock (read-only; atomic-rename writes keep reads consistent).
func loadCacheMap(path string) (map[string]cachedCred, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m map[string]cachedCred
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// collectCacheEntries reads token-cache.yaml at path and returns conv's
// result for every unexpired entry, applying the shared per-entry rules
// (expiry skip, legacy name-from-key fallback) in the single, preallocated
// iteration both readers share. The bearer token is passed alongside the
// metadata; a conv that does not want tokens simply ignores the argument,
// so tokens never propagate past the unmarshal on that path.
func collectCacheEntries[T any](path string, conv func(info CachedCredInfo, token string) T) ([]T, error) {
	m, err := loadCacheMap(path)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]T, 0, len(m))
	for key, cred := range m {
		if !cred.Expiry.IsZero() && now.After(cred.Expiry) {
			continue
		}
		name := cred.Name
		if name == "" {
			name = key
		}
		out = append(out, conv(CachedCredInfo{
			Key:     key,
			Name:    name,
			Server:  cred.Server,
			Cluster: cred.Cluster,
			Expiry:  cred.Expiry,
		}, cred.Token))
	}
	return out, nil
}

// readCacheROFromFile reads token-cache.yaml at path and returns unexpired
// entries. Bearer tokens never propagate past the unmarshal on this path.
func readCacheROFromFile(path string) ([]CachedCredInfo, error) {
	return collectCacheEntries(path, func(info CachedCredInfo, _ string) CachedCredInfo {
		return info
	})
}

// ReadOnlyCachedCred returns the token and expiry for a single cache entry by
// key, reading token-cache.yaml directly without loading config.yaml and
// without pruning expired entries. Returns ErrNoToken when the entry is absent
// or expired. Used by the legacy token shim's fast-path probe so that a broken
// config.yaml can never block a warm cache hit.
//
// Expired shim slots are pruned only by store-backed readers (ListCachedCreds
// and the list-tokens/completion path) — a deliberate bounded-growth tradeoff
// for the config-free fast path: the shim's dominant invocation is a warm
// cache hit, and pruning requires a config.yaml read (for the store policy)
// that the fast path exists specifically to avoid.
func ReadOnlyCachedCred(dir, key string) (string, time.Time, error) {
	path := filepath.Join(dir, "token-cache.yaml")
	// Open once and use handle.Stat() for the permission warning so this
	// fast path — a fresh OS process on every kubectl exec-credential
	// refresh — pays a single syscall instead of two (os.ReadFile + os.Stat).
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", time.Time{}, ErrNoToken
		}
		return "", time.Time{}, err
	}
	defer func() { _ = f.Close() }()

	// Warn on loose permissions like fileStore.readCache does — this is the
	// shim's dominant read path, and unlike the completion readers sharing
	// loadCacheMap, its stderr is not TAB-completion output.
	if fi, serr := f.Stat(); serr == nil {
		warnLoosePermFI(path, fi)
	}

	b, rerr := io.ReadAll(f)
	if rerr != nil {
		return "", time.Time{}, rerr
	}
	var m map[string]cachedCred
	if uerr := yaml.Unmarshal(b, &m); uerr != nil {
		return "", time.Time{}, uerr
	}
	cred, ok := m[key]
	if !ok {
		return "", time.Time{}, ErrNoToken
	}
	if !cred.Expiry.IsZero() && time.Now().After(cred.Expiry) {
		return "", time.Time{}, ErrNoToken
	}
	return cred.Token, cred.Expiry, nil
}

// ReadFileTokenSilent reads the login token for serverName from credentials.yaml
// WITHOUT calling warnLoosePerm (AC-23, finding 8). Used only on completion paths
// where any stderr output would appear as a TAB candidate.
func ReadFileTokenSilent(dir, serverName string) string {
	b, err := os.ReadFile(filepath.Join(dir, "credentials.yaml"))
	if err != nil {
		return ""
	}
	var m map[string]string
	if err := yaml.Unmarshal(b, &m); err != nil {
		return ""
	}
	return m[serverName]
}

// CachedCredEntry is CachedCredInfo with the bearer token included.
// Used on the completion path (finding 8) so the token is propagated through
// the first file read, avoiding a second parse of token-cache.yaml.
// CachedCredInfo is kept unchanged for its other callers.
type CachedCredEntry struct {
	CachedCredInfo
	Token string
}

// ReadOnlyCachedCredsForClusterWithTokens returns unexpired entries matching
// serverHost and cluster, each including its bearer token. This allows a single
// token-cache.yaml read to supply both entry metadata and the token (finding 8).
func ReadOnlyCachedCredsForClusterWithTokens(dir, serverHost, cluster string) ([]CachedCredEntry, error) {
	all, err := readCacheROWithTokensFromFile(filepath.Join(dir, "token-cache.yaml"))
	if err != nil {
		return nil, err
	}
	var out []CachedCredEntry
	for _, e := range all {
		if e.Server == serverHost && e.Cluster == cluster {
			out = append(out, e)
		}
	}
	return out, nil
}

// readCacheROWithTokensFromFile reads token-cache.yaml and returns unexpired
// entries including their bearer tokens. Never writes to disk.
func readCacheROWithTokensFromFile(path string) ([]CachedCredEntry, error) {
	return collectCacheEntries(path, func(info CachedCredInfo, token string) CachedCredEntry {
		return CachedCredEntry{CachedCredInfo: info, Token: token}
	})
}
