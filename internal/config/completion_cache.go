package config

import (
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

// ReadOnlyCachedCredsForCluster returns unexpired entries matching serverHost
// and cluster, without writing. When multiple entries match, the caller must
// treat the result as ambiguous (AC-6a: exactly-one rule).
func ReadOnlyCachedCredsForCluster(dir, serverHost, cluster string) ([]CachedCredInfo, error) {
	all, err := ReadOnlyCachedCreds(dir)
	if err != nil {
		return nil, err
	}
	var out []CachedCredInfo
	for _, info := range all {
		if info.Server == serverHost && info.Cluster == cluster {
			out = append(out, info)
		}
	}
	return out, nil
}

// readCacheROFromFile reads token-cache.yaml at path and returns unexpired
// entries. It never writes to disk regardless of expired entries found.
func readCacheROFromFile(path string) ([]CachedCredInfo, error) {
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
	now := time.Now()
	out := make([]CachedCredInfo, 0, len(m))
	for key, cred := range m {
		if !cred.Expiry.IsZero() && now.After(cred.Expiry) {
			continue // skip expired; do NOT write
		}
		name := cred.Name
		if name == "" {
			name = key // legacy bare entry
		}
		out = append(out, CachedCredInfo{
			Key:     key,
			Name:    name,
			Server:  cred.Server,
			Cluster: cred.Cluster,
			Expiry:  cred.Expiry,
		})
	}
	return out, nil
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

// ReadOnlyTokenForKey returns the token stored under key in token-cache.yaml,
// or "" when absent, expired, or unreadable. Never writes (AC-27).
func ReadOnlyTokenForKey(dir, key string) string {
	b, err := os.ReadFile(filepath.Join(dir, "token-cache.yaml"))
	if err != nil {
		return ""
	}
	var m map[string]cachedCred
	if err := yaml.Unmarshal(b, &m); err != nil {
		return ""
	}
	cred, ok := m[key]
	if !ok {
		return ""
	}
	if !cred.Expiry.IsZero() && time.Now().After(cred.Expiry) {
		return ""
	}
	return cred.Token
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
	now := time.Now()
	out := make([]CachedCredEntry, 0, len(m))
	for key, cred := range m {
		if !cred.Expiry.IsZero() && now.After(cred.Expiry) {
			continue
		}
		name := cred.Name
		if name == "" {
			name = key
		}
		out = append(out, CachedCredEntry{
			CachedCredInfo: CachedCredInfo{
				Key:     key,
				Name:    name,
				Server:  cred.Server,
				Cluster: cred.Cluster,
				Expiry:  cred.Expiry,
			},
			Token: cred.Token,
		})
	}
	return out, nil
}
