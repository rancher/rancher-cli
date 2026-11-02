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

// TestHostEntryName asserts the shared derivation: the host after the
// prefix, the port appended with a dash when the URL names one, and never
// a colon (an IPv6 literal's colons become dashes).
func TestHostEntryName(t *testing.T) {
	t.Parallel()
	cases := []struct{ url, want string }{
		{"https://rancher.example.com", "p-rancher.example.com"},
		{"https://rancher.example.com:8443/", "p-rancher.example.com-8443"},
		{"https://RANCHER.example.com", "p-RANCHER.example.com"},
		{"https://[2001:db8::1]:8443", "p-2001-db8--1-8443"},
		{"https://[fe80::1%25eth0]:8443", "p-fe80--1%eth0-8443"},
		{"::not a url", "p-unknown"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, HostEntryName("p-", c.url), c.url)
	}
}

// TestRecoveryEntryName asserts the recovery entry carries the recover-
// prefix over the shared derivation. Host case is preserved: the entry
// name is a label, and the URL it binds to is canonical.
func TestRecoveryEntryName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "recover-rancher.example.com-8443", RecoveryEntryName("https://rancher.example.com:8443/"))
	assert.Equal(t, "recover-unknown", RecoveryEntryName("::not a url"))
}

// expandedSequence is the recovery sequence as an invocation with the
// default directory prints it.
func expandedSequence(serverURL string, ref TokenRef) []string {
	seq := RecoverySequence(serverURL, ref)
	out := make([]string, len(seq))
	for i, c := range seq {
		out[i] = ExpandRemedies(c, "")
	}
	return out
}

// TestRecoverySequenceBindsLoginAndDeleteToTheRecordedURL asserts the four
// commands, in order, every dynamic value shell-quoted, the kind-aware delete
// from RecoveryCommands, and the closing logout; each opens with the marker,
// and no command holds a comment.
func TestRecoverySequenceBindsLoginAndDeleteToTheRecordedURL(t *testing.T) {
	t.Parallel()
	got := RecoverySequence("https://r.ex:8443", TokenRef{Kind: TokenKindV3, Name: "kubeconfig-u-x"})
	require.Len(t, got, 4)
	for _, c := range got {
		assert.True(t, strings.HasPrefix(c, Rancher+" "), "%q opens with the marker", c)
		assert.NotContains(t, c, "#")
	}
	assert.Equal(t, []string{
		"rancher auth login 'recover-r.ex-8443' --url 'https://r.ex:8443'",
		"rancher config use-server 'recover-r.ex-8443'",
		"rancher delete management-token 'kubeconfig-u-x' --apply --yes --allow-deprecated",
		"rancher auth logout 'recover-r.ex-8443'",
	}, expandedSequence("https://r.ex:8443", TokenRef{Kind: TokenKindV3, Name: "kubeconfig-u-x"}))

	ext := expandedSequence("https://r.ex", TokenRef{Kind: TokenKindExt, Name: "token-1"})
	assert.Equal(t, "rancher auth login 'recover-r.ex' --url 'https://r.ex'", ext[0])
	assert.Equal(t, "rancher delete token 'token-1' --apply --yes", ext[2])
	assert.Contains(t, RecoverySequenceAssumption, "RANCHER_CLI_URL")
	assert.Contains(t, RecoverySequenceAssumption, "RANCHER_CLI_TOKEN")
}

// TestRecoverySequenceQuotesHostileValues asserts values from persisted or
// server data cannot break out of their argument: whitespace, quotes, shell
// metacharacters and an IPv6 zone in the URL all round-trip as one argument
// each.
func TestRecoverySequenceQuotesHostileValues(t *testing.T) {
	t.Parallel()
	// net/url decodes the RFC 6874 zone: Hostname() of [fe80::1%25eth0] is
	// fe80::1%eth0, so the derived entry carries %eth0 while the --url value
	// is the input string unchanged.
	got := expandedSequence("https://[fe80::1%25eth0]:8443/;rm", TokenRef{Kind: TokenKindExt, Name: "token-1"})
	assert.Equal(t, "rancher auth login 'recover-fe80--1%eth0-8443' --url 'https://[fe80::1%25eth0]:8443/;rm'", got[0])
	assert.Equal(t, "rancher config use-server 'recover-fe80--1%eth0-8443'", got[1])
	for _, line := range got {
		assert.Equal(t, 1, strings.Count(line, "rancher "), "one rancher invocation per line: %s", line)
	}
	// Round trip: a POSIX shell splits each quoted value back to itself.
	assert.Equal(t, []string{"rancher", "auth", "login", "recover-fe80--1%eth0-8443", "--url", "https://[fe80::1%25eth0]:8443/;rm"}, shellFields(t, got[0]))
	assert.Equal(t, []string{"rancher", "config", "use-server", "recover-fe80--1%eth0-8443"}, shellFields(t, got[1]))
}

// TestRecoverySequenceQuotesADashedName asserts a token name that starts
// with a dash is written after "--" so the delete reads it as a name, and the
// recovery entry, which always opens with "recover-", needs no such form.
func TestRecoverySequenceQuotesADashedName(t *testing.T) {
	t.Parallel()
	got := expandedSequence("https://-x.example.com", TokenRef{Kind: TokenKindExt, Name: "-t"})
	assert.Equal(t, "rancher auth login 'recover--x.example.com' --url 'https://-x.example.com'", got[0])
	assert.Equal(t, "rancher config use-server 'recover--x.example.com'", got[1])
	assert.Equal(t, "rancher delete token --apply --yes -- '-t'", got[2])
}
