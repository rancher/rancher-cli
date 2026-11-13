package clierr

import (
	"net/url"
	"strings"
)

// RecoverySequenceAssumption is the sentence every rendering of the
// recovery sequence carries: the sequence binds the delete to the recorded
// server through the current-server pointer, which an environment override
// would win over.
const RecoverySequenceAssumption = "assumes RANCHER_CLI_URL and RANCHER_CLI_TOKEN are unset; either would override the current server the sequence selects"

// RecoveryEntryName is the server entry the recovery sequence logs in
// under: recover-<host>, with -<port> appended when the URL names one, and
// every colon replaced by a dash so the name never reads as a URL
// fragment. It is a convention in the rendered text, not a reserved
// prefix; a later recovery for the same host reuses the entry, and a
// recover-<host> that already names a different URL is refused by auth
// login with its update-server hint.
func RecoveryEntryName(serverURL string) string { return HostEntryName("recover-", serverURL) }

// HostEntryName derives an entry name from a server URL: the prefix, the
// host, and -<port> when the URL names one, with every colon replaced by a
// dash so an IPv6 literal yields a plain label. A URL that does not parse
// or names no host yields <prefix>unknown.
func HostEntryName(prefix, serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil || u.Hostname() == "" {
		return prefix + "unknown"
	}
	name := prefix + u.Hostname()
	if p := u.Port(); p != "" {
		name += "-" + p
	}
	return strings.ReplaceAll(name, ":", "-")
}

// RecoverySequence is the four commands that revoke one token whose entry is
// not the current server: log in to the recorded URL under the recovery
// entry, select that entry, run the kind-correct delete, and log the recovery
// entry out again since its login created a CLI-owned token of its own.
// Rendered as text, in warnings, and in result data; never as a hint, which
// holds one command. A value that cannot be printed leaves the whole sequence
// out; its text then gives the list command of the token. The caller names
// the server with On.
func RecoverySequence(serverURL string, ref TokenRef) Sequence {
	entry := RecoveryEntryName(serverURL)
	return NewSequence().
		Then("", NewCommand("auth login").Arg(entry).FlagValue("--url", serverURL)).
		Then("", NewCommand("config use-server").Arg(entry)).
		Then("", Revoke(ref)).
		Then("", NewCommand("auth logout").Arg(entry))
}
