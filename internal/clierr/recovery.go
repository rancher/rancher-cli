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

// RecoverySequence renders the four commands that revoke one token whose
// entry is not the current server: log in to the recorded URL under the
// recovery entry, select that entry, run the kind-correct delete, and log the
// recovery entry out again since its login created a CLI-owned token of its
// own. Rendered as text, in warnings, and in result data; never as a hint,
// which holds one command. Every value that came from persisted or server
// data is shell-quoted; a stray metacharacter or newline in a name cannot
// turn display text into a command. Each command is a remedy: it names the
// config directory when the invocation was given one.
func RecoverySequence(serverURL string, ref TokenRef) []string {
	entry := RecoveryEntryName(serverURL)
	_, revoke := RecoveryCommands(ref)
	return []string{
		Rancher + " auth login" + ShellQuoteArgs(" --url "+ShellQuote(serverURL), entry),
		Rancher + " config use-server" + ShellQuoteArgs("", entry),
		revoke,
		Rancher + " auth logout" + ShellQuoteArgs("", entry),
	}
}
