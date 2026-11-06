package config

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unusableURLs are server URLs that do not canonicalize, each with a password
// the tests look for.
var unusableURLs = []struct{ name, in string }{
	{"a bad port", "https://u:pwzq7@Hostzq7:abc"},
	{"no scheme", "u:pwzq7@hostzq7:1"},
	{"a user name alone", "https://u@hostzq7:abc"},
	{"a slash in the password", "https://u:pwzq7/w@hostzq7:abc"},
	{"a space in the user name", "https://a b:pwzq7@Hostzq7:443"},
	{"a colon in the scheme position", "x@y://pwzq7@hostzq7:abc"},
	{"an at sign in the password", "https://u:pw@zq7@hostzq7:abc"},
	{"a query", "https://hostzq7:abc?e=pwzq7@other"},
}

// TestDisplayServerURL asserts the display form of a server URL: scheme and
// host for a URL that parses, whatever userinfo, path, query and fragment it
// holds; the same form of the text left after the userinfo is removed for one
// that does not parse; "<unusable URL>" otherwise.
func TestDisplayServerURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"plain", "https://hostzq7", "https://hostzq7"},
		{"a port", "https://hostzq7:8443", "https://hostzq7:8443"},
		{"userinfo", "https://u:pwzq7@hostzq7:8443/pathzq7?q=1#f", "https://hostzq7:8443"},
		{"an at sign in the path", "https://hostzq7/p@th", "https://hostzq7"},
		{"a space in the user name", "https://a b:pwzq7@Hostzq7:443", "https://Hostzq7:443"},
		{"a bad port with userinfo", "https://u:pwzq7@hostzq7:abc", "<unusable URL>"},
		{"no scheme", "u:pwzq7@hostzq7:1", "<unusable URL>"},
		{"a slash in the password", "https://u:pwzq7/w@hostzq7:abc", "<unusable URL>"},
		{"an identity of an unusable URL", "unusable:https://hostzq7:abc", "<unusable URL>"},
		{"a bare host", "hostzq7", "<unusable URL>"},
		{"an at sign in the query of a bad port", "https://hostzq7:abc?e=pwzq7@other", "<unusable URL>"},
		{"an at sign in the path of a bad port", "https://hostzq7:abc/p@th", "<unusable URL>"},
		{"an at sign in the query of a bad escape", "https://hostzq7/a%zz?q=1@leakzq7", "https://hostzq7"},
		{"an at sign in the path of an IPv6 host with a bad port", "https://[::1]:abc/p@q", "<unusable URL>"},
		{"an at sign in the fragment after a bad escape", "https://hostzq7/%zz#a@leakzq7", "https://hostzq7"},
		{"a slash in the user information", "https://u:p/x@hostzq7", "<unusable URL>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DisplayServerURL(tc.in)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "@")
			assert.NotContains(t, got, "pwzq7")
			assert.NotContains(t, got, "leakzq7")
		})
	}
}

// TestCanonicalServerIdentity asserts the identity of a URL that
// canonicalizes (unchanged) and the fallback for one that does not: no user
// information survives, hosts that differ give different identities, URLs
// that differ only in their user information give one, a second pass gives
// the same text, and no fallback equals the identity of a usable server.
func TestCanonicalServerIdentity(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "https://hostzq7", CanonicalServerIdentity("HTTPS://u:pwzq7@Hostzq7:443/p"))
	assert.Empty(t, CanonicalServerIdentity(""), "no server recorded stays empty")
	for _, tc := range unusableURLs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id := CanonicalServerIdentity(tc.in)
			assert.True(t, strings.HasPrefix(id, "unusable:"), id)
			assert.NotContains(t, id, "@")
			assert.NotContains(t, id, "pwzq7")
			assert.Equal(t, id, CanonicalServerIdentity(id), "a second pass returns the same text")
			_, err := credhelper.CanonicalServerURL(id)
			assert.Error(t, err, "a fallback identity is never a canonical one")
		})
	}
}

// TestCanonicalServerIdentityFallbackRows asserts the exact fallback for the
// rows that decide the algorithm.
func TestCanonicalServerIdentityFallbackRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"a bad port", "https://u:pw@Host:abc", "unusable:https://host:abc"},
		{"no parseable host", "u:pw@host:1", "unusable:host:1"},
		{"a raw slash in the password", "https://u:p/w@h:abc", "unusable:https://h:abc"},
		{"a space in the user name", "https://a b:pw@Host:443", "unusable:https://host:443"},
		{"an at sign before a scheme separator", "x@y://z@w:abc", "unusable:w:abc"},
		{"a schemeless input with a separator in its path", "host:abc/x://y@z", "unusable:z"},
		{"a hand-edited identity with userinfo", "unusable:https://u:pw@h:abc", "unusable:h:abc"},
		{"a hand-edited identity without userinfo", "unusable:https://h:abc", "unusable:https://h:abc"},
		{"an at sign in the query", "https://h:abc/x?e=a@b", "unusable:https://b"},
		{"userinfo only", "https://u:pw@", "unusable:https://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, CanonicalServerIdentity(tc.in))
		})
	}
}

// TestCanonicalServerIdentityFallbackKeepsServersApart asserts that two
// unusable URLs with different hosts get different identities, that two that
// differ only in their user information get one, that two hosts that share an
// "@" in their path share one (such a URL cannot be stored by the CLI), and
// that an unusable URL never takes the identity of a usable server.
func TestCanonicalServerIdentityFallbackKeepsServersApart(t *testing.T) {
	t.Parallel()
	assert.NotEqual(t, CanonicalServerIdentity("https://u:pw@h1:abc"), CanonicalServerIdentity("https://u:pw@h2:abc"))
	assert.Equal(t, CanonicalServerIdentity("https://u:pw@h1:abc"), CanonicalServerIdentity("https://other:secret@h1:abc"))
	assert.Equal(t, CanonicalServerIdentity("https://host1:abc/p@th"), CanonicalServerIdentity("https://host2:abc/p@th"))
	assert.NotEqual(t, CanonicalServerIdentity("https://x:abc/p@good.example.com"), CanonicalServerIdentity("https://good.example.com"))
	assert.Equal(t, "unusable:https://good.example.com", CanonicalServerIdentity("https://x:abc/p@good.example.com"))
}

// TestServerURLOriginTexts asserts the refusal of a server URL names where it
// came from, says why, tells how to repair it for each origin, and holds no
// part of the URL.
func TestServerURLOriginTexts(t *testing.T) {
	t.Parallel()
	_, userinfo := credhelper.CheckServerURL("https://u:pwzq7@hostzq7")
	rancher := clierr.Rancher
	const form = "the server URL in "
	for _, tc := range []struct {
		name   string
		origin ServerURLOrigin
		reason error
		want   string
	}{
		{"a typed flag", ServerURLOrigin{Source: "--url"}, userinfo,
			form + "--url cannot be used: it holds a user name or password; give a URL like https://host, without a user name or password"},
		{"the variable", ServerURLOrigin{Source: EnvURL}, userinfo,
			form + EnvURL + " cannot be used: it holds a user name or password; give a URL like https://host, without a user name or password"},
		{"a flag and a scheme", ServerURLOrigin{Source: "--url"}, ErrServerURLScheme,
			form + "--url cannot be used: its scheme is not http or https; use http:// or https://"},
		{"the server flag of a command kubectl runs", ServerURLOrigin{Source: "--server", ExecEntryArgument: true}, userinfo,
			form + "--server cannot be used: it holds a user name or password; give a URL like https://host, without a user name or password; " +
				"when kubectl runs this command, the URL is the --server argument of the kubeconfig's exec block"},
		{"an ordinary entry", EntryOrigin("prod"), userinfo,
			form + "server entry 'prod' cannot be used: it holds a user name or password; change the entry's URL with " +
				rancher + " config update-server 'prod' --url <url>"},
		{"an ordinary entry and a scheme", EntryOrigin("prod"), ErrServerURLScheme,
			form + "server entry 'prod' cannot be used: its scheme is not http or https; change the entry's URL with " +
				rancher + " config update-server 'prod' --url <url>"},
		{"an exec plugin entry", EntryOrigin(ExecLoginPrefix + "abc"), userinfo,
			form + "server entry '" + ExecLoginPrefix + "abc' cannot be used: it holds a user name or password; " +
				"if the entry holds a login, run: " + rancher + " auth logout '" + ExecLoginPrefix + "abc'; then remove the entry with: " + rancher + " config delete-server '" + ExecLoginPrefix + "abc'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.origin.Text(tc.reason))
			ce := tc.origin.Refusal(tc.reason)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
			assert.Empty(t, ce.Hint, "the repair holds a placeholder: no hint")
			assert.NotContains(t, ce.Message, "pwzq7")
			assert.NotContains(t, ce.Message, "hostzq7")
		})
	}
}

// TestAdviseRecord asserts the one decision about a token record's server, in
// order: no server recorded; the entry's repair while the entry exists, when
// the record's server is its own URL and that URL fails the check (with the
// reason of the URL as stored); the token stays when the identity does not
// canonicalize (the reason is the entry's own URL's when the record is at it)
// or when a stored user name or password sits on a scheme other than http and
// https; else the recovery sequence built from the identity, never from the
// text as stored. A plain server with another scheme is not judged by it.
func TestAdviseRecord(t *testing.T) {
	t.Parallel()
	ref := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}
	execName := ExecLoginPrefix + "abc"
	stays := func(reason string) string {
		return "the server recorded for token 'token-abc12' cannot be used: " + reason + "; the token stays on the server that issued it; " +
			"delete it there as user 'u-abc12' while signed in to that server: " + clierr.Rancher + " delete token 'token-abc12' --apply --yes"
	}
	sequence := func(server string) []string { return clierr.RecoverySequence(server, ref) }
	const userinfo = "it holds a user name or password"
	for _, tc := range []struct {
		name             string
		site             RecordSite
		recordServer     string
		wantText         string
		wantSequenceFrom string
	}{
		{"a record at the entry's URL plus userinfo, on a usable entry", RecordSite{Entry: "prod", EntryURL: "https://hostzq7"}, "https://u:pwzq7@hostzq7", "", "https://hostzq7"},
		{"the same on a usable plugin entry", RecordSite{Entry: execName, EntryURL: "https://hostzq7"}, "https://u:pwzq7@hostzq7", "", "https://hostzq7"},
		{"a record at another port plus userinfo", RecordSite{Entry: "prod", EntryURL: "https://hostzq7"}, "https://u:pwzq7@hostzq7:8443", "", "https://hostzq7:8443"},
		{"a record the identity of which is usable", RecordSite{Entry: "prod", EntryURL: "https://hostzq7"}, "https://other.example", "", "https://other.example"},
		{"a record that does not canonicalize, on a usable entry", RecordSite{Entry: "prod", EntryURL: "https://hostzq7"}, "https://u:pwzq7@other:abc", stays("it is not a valid URL"), ""},
		{"the same on a usable plugin entry", RecordSite{Entry: execName, EntryURL: "https://hostzq7"}, "https://u:pwzq7@other:abc", stays("it is not a valid URL"), ""},
		{"no entry", RecordSite{}, "unusable:https://other:abc", stays("it is not a valid URL"), ""},
		{"an entry whose own URL holds userinfo", RecordSite{Entry: "prod", EntryURL: "https://u:pwzq7@hostzq7"}, "https://u:pwzq7@hostzq7",
			EntryOrigin("prod").Text(mustProblem(t, "https://u:pwzq7@hostzq7")), ""},
		{"the identity of an entry whose own URL holds userinfo", RecordSite{Entry: "prod", EntryURL: "https://u:pwzq7@hostzq7"}, "https://hostzq7",
			EntryOrigin("prod").Text(mustProblem(t, "https://u:pwzq7@hostzq7")), ""},
		{"a plugin entry whose own URL fails", RecordSite{Entry: execName, EntryURL: "https://u:pwzq7@hostzq7:abc"}, "unusable:https://hostzq7:abc",
			EntryOrigin(execName).Text(mustProblem(t, "https://u:pwzq7@hostzq7:abc")), ""},
		{"a removed entry whose URL failed to parse: the reason of that URL", RecordSite{EntryURL: "https://u:pwzq7@hostzq7:abc", Removed: true}, "unusable:https://hostzq7:abc", stays("it is not a valid URL"), ""},
		{"a removed schemeless entry: one reason", RecordSite{EntryURL: "u:pwzq7@hostzq7:1", Removed: true}, "unusable:hostzq7:1", stays("it must be absolute (scheme://host)"), ""},
		{"a removed entry whose URL holds userinfo: the sequence", RecordSite{Entry: "prod", EntryURL: "https://u:pwzq7@hostzq7", Removed: true}, "https://hostzq7", "", "https://hostzq7"},
		{"the same with a user name alone", RecordSite{EntryURL: "https://userzq7@hostzq7", Removed: true}, "https://hostzq7", "", "https://hostzq7"},
		{"a record dropped on an entry that exists: the text for the record", RecordSite{EntryURL: "https://u:pwzq7@hostzq7"}, "https://hostzq7", stays(userinfo), ""},
		{"a record with userinfo on another scheme", RecordSite{Entry: "prod", EntryURL: "https://hostzq7"}, "ftp://u:pwzq7@hostzq7:21", stays("its scheme is not http or https"), ""},
		{"a removed entry with userinfo on another scheme: the reason of its URL", RecordSite{EntryURL: "ftp://u:pwzq7@hostzq7:21", Removed: true}, "ftp://hostzq7:21",
			stays(mustProblem(t, "ftp://u:pwzq7@hostzq7:21").Error()), ""},
		{"a plain record on another scheme", RecordSite{Entry: "prod", EntryURL: "https://hostzq7"}, "ftp://hostzq7:21", "", "ftp://hostzq7:21"},
		{"an entry whose own URL is plain on another scheme", RecordSite{Entry: "prod", EntryURL: "ftp://hostzq7:21"}, "ftp://hostzq7:21", "", "ftp://hostzq7:21"},
		{"an entry whose own URL has userinfo on another scheme", RecordSite{Entry: "prod", EntryURL: "ftp://u:pwzq7@hostzq7:21"}, "ftp://u:pwzq7@hostzq7:21",
			EntryOrigin("prod").Text(mustProblem(t, "ftp://u:pwzq7@hostzq7:21")), ""},
		{"a record that names no server", RecordSite{Entry: "prod", EntryURL: "https://hostzq7"}, "",
			"no server is recorded for token 'token-abc12'; delete it as user 'u-abc12', signed in to the server that issued it: " + clierr.Rancher + " delete token 'token-abc12' --apply --yes", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := AdviseRecord(tc.site, tc.recordServer, "u-abc12", ref)
			assert.Equal(t, tc.wantText, got.Text)
			if tc.wantText == "" {
				assert.Equal(t, sequence(tc.wantSequenceFrom), got.Sequence)
				assert.Equal(t, "u-abc12", got.LoginAs)
				assert.NotContains(t, strings.Join(got.Sequence, " "), "pwzq7", "no command is built from the text as stored")
			} else {
				assert.Empty(t, got.Sequence)
				assert.Empty(t, got.LoginAs)
				assert.NotContains(t, got.Text, "pwzq7")
			}
		})
	}
}

// TestAdviseRecordNamesTheOwnerWithoutUnsafeRunes asserts LoginAs is the owner
// id with every control character, invisible format character and line or
// paragraph separator removed: a newline in a stored id cannot end a line of
// the text that names it, a direction override cannot make it read as another
// id, and LoginAs is empty for an unknown owner.
func TestAdviseRecordNamesTheOwnerWithoutUnsafeRunes(t *testing.T) {
	t.Parallel()
	ref := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}
	site := RecordSite{Entry: "prod", EntryURL: "https://hostzq7"}
	assert.Equal(t, "u-a$(id)bc", AdviseRecord(site, "https://hostzq7", "u-a\n$(id)\x1bbc", ref).LoginAs)
	assert.Equal(t, "u-abc", AdviseRecord(site, "https://hostzq7", "u-a\u202eb\u2028c", ref).LoginAs)
	assert.Empty(t, AdviseRecord(site, "https://hostzq7", "", ref).LoginAs)
}

// TestAdviseRecordNamesNoUserForAnOwnerOfOnlyUnsafeRunes asserts an owner id
// made only of invisible format characters is treated as an unknown owner:
// LoginAs is empty, and a text that names who runs the delete says its owner
// or an administrator, never an empty user name.
func TestAdviseRecordNamesNoUserForAnOwnerOfOnlyUnsafeRunes(t *testing.T) {
	t.Parallel()
	ref := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}
	const owner = "\u202e\u2063"
	site := RecordSite{Entry: "prod", EntryURL: "https://hostzq7"}
	advice := AdviseRecord(site, "https://hostzq7", owner, ref)
	assert.Empty(t, advice.LoginAs)
	assert.Empty(t, advice.Text)
	assert.NotEmpty(t, advice.Sequence)

	noServer := AdviseRecord(site, "", owner, ref)
	assert.Empty(t, noServer.LoginAs)
	assert.Contains(t, noServer.Text, "delete it as "+RecordOwnerOrAdministrator+", signed in")
	assert.NotContains(t, noServer.Text, "user ")
}

// mustProblem is the reason the check gives for raw.
func mustProblem(t *testing.T, raw string) error {
	t.Helper()
	err := CheckServerURL(raw)
	require.Error(t, err)
	return err
}

// TestCAFileProblemNamesNoPath asserts the CA file failure text names the kind
// of failure and no part of the path.
func TestCAFileProblemNamesNoPath(t *testing.T) {
	t.Parallel()
	_, err := ResolveCA("/nonexistent/pathzq7.pem")
	require.Error(t, err)
	assert.Equal(t, "the file does not exist", CAFileProblem(err))
	assert.Equal(t, "the file cannot be read", CAFileProblem(&fs.PathError{Op: "open", Path: "/pathzq7", Err: fs.ErrPermission}))
	assert.Equal(t, "the file cannot be read", CAFileProblem(errors.New("a directory")))
}

// TestErrNotLoggedInForLeavesAnUnusableURLOut asserts that the login entry
// of the exec plugin gets a login command for a usable URL and, for an unusable
// one, neither the URL nor a command in the message or the hint.
func TestErrNotLoggedInForLeavesAnUnusableURLOut(t *testing.T) {
	t.Parallel()
	name := ExecLoginPrefix + "entryzq9"
	usable := ErrNotLoggedInFor(map[string]string{name: "https://hostzq7"}, nil)
	assert.Contains(t, usable.Message, "--url 'https://hostzq7'")
	assert.NotEmpty(t, usable.Hint)

	for _, raw := range []string{"https://u:pwzq7@hostzq7", "https://u:pwzq7@hostzq7:abc", "u:pwzq7@hostzq7:1"} {
		err := ErrNotLoggedInFor(map[string]string{name: raw}, map[string]bool{name: true})
		assert.NotContains(t, err.Message, "pwzq7", raw)
		assert.NotContains(t, err.Message, "hostzq7", raw)
		assert.NotContains(t, err.Message, "auth login "+"'", raw)
		assert.Empty(t, err.Hint, raw)
		assert.NotContains(t, err.Message, "config update-server", "the entry's URL cannot change")
		assert.Contains(t, err.Message, "the server URL in server entry '"+name+"' cannot be used: ", raw)
		assert.Contains(t, err.Message, "if the entry holds a login, run: "+clierr.Rancher+" auth logout '"+name+"'; then remove the entry with: "+
			clierr.Rancher+" config delete-server '"+name+"'", raw)
	}
}

// TestDisplayServerIdentity asserts the identity of a URL that does not
// canonicalize shows without its prefix, a bare host as the host, and that an
// identity of a usable URL and an empty one show as they are.
func TestDisplayServerIdentity(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		CanonicalServerIdentity("acme.ex"):                  "acme.ex",
		CanonicalServerIdentity("https://u:pwzq7@h:abc"):    "https://h:abc",
		CanonicalServerIdentity("https://good.example.com"): "https://good.example.com",
		"": "",
	} {
		assert.Equal(t, want, DisplayServerIdentity(in), in)
	}
}
