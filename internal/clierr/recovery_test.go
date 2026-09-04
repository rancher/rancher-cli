package clierr

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shellFields runs `printf '%s\n' <line>` through /bin/sh and returns the
// words the shell produced, proving the quoting round-trips; skipped where
// there is no POSIX shell.
func shellFields(t *testing.T, line string) []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shell")
	}
	out, err := exec.Command("/bin/sh", "-c", "printf '%s\\n' "+strings.TrimPrefix(line, "rancher ")).Output()
	require.NoError(t, err)
	return append([]string{"rancher"}, strings.Split(strings.TrimRight(string(out), "\n"), "\n")...)
}

// TestRecoveryEntryName asserts the recovery entry name is derived from
// the host, carries the port when the URL has one, and never contains a
// colon (an IPv6 literal's colons become dashes).
func TestRecoveryEntryName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "recover-rancher.example.com", RecoveryEntryName("https://rancher.example.com"))
	assert.Equal(t, "recover-rancher.example.com-8443", RecoveryEntryName("https://rancher.example.com:8443/"))
	assert.Equal(t, "recover-RANCHER.example.com", RecoveryEntryName("https://RANCHER.example.com"), "host case is preserved: the entry name is a label, and the URL it binds to is canonical")
	assert.Equal(t, "recover-2001-db8--1-8443", RecoveryEntryName("https://[2001:db8::1]:8443"))
	assert.Equal(t, "recover-fe80--1%eth0-8443", RecoveryEntryName("https://[fe80::1%25eth0]:8443"), "net/url decodes the zone's %25 to %")
	assert.Equal(t, "recover-unknown", RecoveryEntryName("::not a url"))
}

// TestRecoverySequenceBindsLoginAndDeleteToTheRecordedURL asserts the
// four commands, in order, every dynamic value shell-quoted, with the owner
// comment on the login line, the kind-aware delete from RecoveryCommands,
// and the closing logout; the owner comment is omitted only for an empty
// owner.
func TestRecoverySequenceBindsLoginAndDeleteToTheRecordedURL(t *testing.T) {
	t.Parallel()
	got := RecoverySequence("https://r.ex:8443", "user-a", TokenRef{Kind: TokenKindV3, Name: "kubeconfig-u-x"})
	assert.Equal(t, []string{
		"rancher auth login 'recover-r.ex-8443' --url 'https://r.ex:8443'   # log in as user 'user-a'",
		"rancher config use-server 'recover-r.ex-8443'",
		"rancher delete management-token kubeconfig-u-x --apply --yes --allow-deprecated",
		"rancher auth logout 'recover-r.ex-8443'",
	}, got)

	noOwner := RecoverySequence("https://r.ex", "", TokenRef{Kind: TokenKindExt, Name: "token-1"})
	assert.Equal(t, "rancher auth login 'recover-r.ex' --url 'https://r.ex'", noOwner[0])
	assert.Equal(t, "rancher delete token token-1 --apply --yes", noOwner[2])
	assert.Contains(t, RecoverySequenceAssumption, "RANCHER_CLI_URL")
	assert.Contains(t, RecoverySequenceAssumption, "RANCHER_CLI_TOKEN")
}

// TestRecoverySequenceQuotesHostileValues asserts values from persisted or
// server data cannot break out of their argument: whitespace, quotes, shell
// metacharacters, a newline in an owner id, and an IPv6 zone in the URL all
// round-trip as one argument each, and the owner stays inside the comment.
func TestRecoverySequenceQuotesHostileValues(t *testing.T) {
	t.Parallel()
	// net/url decodes the RFC 6874 zone: Hostname() of [fe80::1%25eth0] is
	// fe80::1%eth0, so the derived entry carries %eth0 while the --url value
	// is the input string unchanged.
	got := RecoverySequence("https://[fe80::1%25eth0]:8443/;rm", "u'ser\n$(id)", TokenRef{Kind: TokenKindExt, Name: "token-1"})
	assert.Equal(t, "rancher auth login 'recover-fe80--1%eth0-8443' --url 'https://[fe80::1%25eth0]:8443/;rm'   # log in as user 'u'\\''ser$(id)'", got[0],
		"control characters are stripped from the owner before quoting: a newline would end the comment")
	assert.Equal(t, "rancher config use-server 'recover-fe80--1%eth0-8443'", got[1])
	for _, line := range got {
		assert.Equal(t, 1, strings.Count(line, "rancher "), "one rancher invocation per line: %s", line)
	}
	// Round trip: a POSIX shell splits each quoted value back to itself and
	// drops the comment, proving the owner stayed a comment.
	assert.Equal(t, []string{"rancher", "auth", "login", "recover-fe80--1%eth0-8443", "--url", "https://[fe80::1%25eth0]:8443/;rm"}, shellFields(t, got[0]))
	assert.Equal(t, []string{"rancher", "config", "use-server", "recover-fe80--1%eth0-8443"}, shellFields(t, got[1]))
}
