package config

import (
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// ExecLoginPrefix is the reserved entry-name prefix of a plugin login entry.
// auth login and config add-server refuse names under it, the same way they
// refuse LegacyCachePrefix, so provenance cannot be spoofed by a hand-picked
// name; the persisted provenance field, not the name, is authoritative.
const ExecLoginPrefix = "exec-login-"

// IsExecLoginEntryName reports whether name lies in the reserved namespace.
func IsExecLoginEntryName(name string) bool { return strings.HasPrefix(name, ExecLoginPrefix) }

// ExecLoginEntryName derives the plugin login entry's name from the
// canonical server URL: the prefix, the host, and -<port> when the URL
// names one, with colons replaced by dashes so an IPv6 literal yields a
// plain label. A collision with a different-URL entry is resolved by the
// persistence primitive with a numeric suffix.
func ExecLoginEntryName(canonicalURL string) string {
	return clierr.HostEntryName(ExecLoginPrefix, canonicalURL)
}

// ExecLoginEntryNameFor is the name of the plugin login entry the plugin
// creates for a call with --user-id userID: ExecLoginEntryName, a dash and
// the user id, so one server holds one such entry per user. A call without
// --user-id gets ExecLoginEntryName itself.
func ExecLoginEntryNameFor(canonicalURL, userID string) string {
	name := ExecLoginEntryName(canonicalURL)
	if userID == "" {
		return name
	}
	return name + "-" + userID
}

// ValidateEntryName rejects a server name reserved for the legacy token
// shim's cache slots, the exec plugin's cache slots of calls with
// --user-id, or the exec plugin's own login entries (names are
// born at registration now - add-server and login's delegated add both
// call this before any probe or write). The exec plugin asks it too, so a
// login it names as a remedy is one auth login accepts.
func ValidateEntryName(name string) *clierr.Error {
	switch {
	case IsLegacyCacheSlot(name):
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("server names starting with %q are reserved for the legacy token shim's cache slots; use a different name", LegacyCachePrefix))
	case IsUserIDCacheSlot(name):
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("server names starting with %q are reserved for the exec plugin's cache slots of calls with --user-id; use a different name", UserIDCachePrefix))
	case IsExecLoginEntryName(name):
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("server names starting with %q are reserved for the exec plugin's own login entries; use a different name", ExecLoginPrefix))
	}
	return nil
}
