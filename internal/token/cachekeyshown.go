package token

import (
	"maps"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/config"
)

// misreadEntryURL is the URL of the first entry (by name) that holds an @
// after the host and whose host is the identity of server, and whether there is
// one. Any entry counts, whichever entry the record belongs to: a record of an
// earlier build names the host a URL was read as, and the record's own entry
// may be another one.
func misreadEntryURL(cfg *config.Config, server string) (string, bool) {
	plain := config.DisplayServerURL(server)
	for _, name := range slices.Sorted(maps.Keys(cfg.Servers)) {
		if raw := cfg.Servers[name].URL; config.DisplayStoredRecordServer(raw, server) != plain {
			return raw, true
		}
	}
	return "", false
}

// maskedCacheServer is the form of the server a cache record names that
// output may show, and whether it differs from the stored one: a server that is
// the identity of the URL of an entry that holds an @ after the host shows as
// unusable, because it is the start of a user name and password. A usable
// entry at the same identity does not unmask it here: a record cannot tell
// which of the entries its server was read from, and auth list-tokens, which
// chooses no entry, keeps the mask. A delete's result shows the server of a
// token record recorded on such a usable entry (Deletion.planCacheRecord).
func maskedCacheServer(cfg *config.Config, server string) (string, bool) {
	if raw, masked := misreadEntryURL(cfg, server); masked {
		return config.DisplayStoredRecordServer(raw, server), true
	}
	return config.DisplayServerURL(server), false
}

// maskedCacheKey is key as output may show it: the server a key holds is
// replaced by its masked form, as Deletion.shownKeys does, and a key that
// holds no such server is returned as it is.
func maskedCacheKey(cfg *config.Config, server, key string) string {
	shown, masked := maskedCacheServer(cfg, server)
	if !masked {
		return key
	}
	return strings.Replace(key, config.CanonicalServerIdentity(server), shown, 1)
}

// maskedMatch is what a key selects only in the masked form: how many
// records, and the one record name they all have when there is one and that
// name selects exactly those records.
type maskedMatch struct {
	n int
	// name is the shared record name, set only when every matched record has
	// it and no other record has it.
	name string
}

// maskedOnlyMatches is which cached records the key name selects only in the
// masked form list-tokens shows, when the stored form of name (or a record
// name) selects none: records that differ only in the server they name show
// one key, and such a key cannot say which it means. The result is empty when
// a stored key or a name matches, or when the cache cannot be read.
func maskedOnlyMatches(store config.CredStore, cfg *config.Config, name string) maskedMatch {
	var m maskedMatch
	var matched []string
	named := map[string]int{}
	err := store.HoldCachedCreds(func(recs []config.CachedRecord) error {
		for _, r := range recs {
			if r.Name == name || r.Key == name {
				m, matched = maskedMatch{}, nil
				return nil
			}
			named[r.Name]++
			if maskedCacheKey(cfg, r.Server, r.Key) == name {
				matched = append(matched, r.Name)
			}
		}
		return nil
	})
	if err != nil || len(matched) == 0 {
		return maskedMatch{}
	}
	m.n = len(matched)
	if names := slices.Compact(slices.Sorted(slices.Values(matched))); len(names) == 1 && named[names[0]] == m.n {
		m.name = names[0]
	}
	return m
}
