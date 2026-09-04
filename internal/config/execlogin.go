package config

import (
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
