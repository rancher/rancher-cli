package config

import "strings"

// Identification is what an exec entry's --name means in one config load:
// the server entry it identifies, or none.
type Identification struct {
	// Entry is the identified entry, empty for none.
	Entry string
	// OtherURL reports that Entry is registered at another URL than the
	// call's server. The call is then refused, never answered by the
	// entries registered at its URL.
	OtherURL bool
}

// IdentifyEntry returns the server entry an exec entry's --name means in
// cfg. The name is the entry itself, or the "<entry>-<cluster>" form
// rancher kubeconfig writes with the same --cluster: an exact entry name
// wins; else, with a cluster, the entry named by the name less
// "-<cluster>". Each step yields at most one entry, and nothing else about
// a name identifies an entry - a name that merely begins with another
// entry's name means that entry no more than any other. Every entry in the
// directory is looked at, not only those that answer for server.
//
// The legacy `rancher token` shim never identifies an entry: its name is a
// cache-slot label.
//
// This is the one place a name becomes an entry. The exec plugin's
// credential lookup, its CA and the cache record it writes use the answer,
// and a cache hit runs it again over its own config load, so a record is
// never served once the name means another entry.
func IdentifyEntry(cfg *Config, server, name, cluster string, legacyShim bool) Identification {
	if legacyShim || name == "" {
		return Identification{}
	}
	entry := ""
	if _, ok := cfg.Servers[name]; ok {
		entry = name
	} else if cluster != "" {
		if e, found := strings.CutSuffix(name, "-"+cluster); found {
			if _, ok := cfg.Servers[e]; ok {
				entry = e
			}
		}
	}
	if entry == "" {
		return Identification{}
	}
	return Identification{Entry: entry, OtherURL: !SameServerURL(cfg.Servers[entry].URL, server)}
}
