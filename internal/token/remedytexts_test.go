package token

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
)

// TestDeleteTokenRefusalNamesBothCommands asserts the usage error of a name
// together with --all gives the delete of that one name and the delete of
// every cached credential, each ending its clause, with the name quoted and,
// for a name that starts with a dash, written after "--".
func TestDeleteTokenRefusalNamesBothCommands(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"prod": "rancher auth delete-token 'prod'",
		"-x":   "rancher auth delete-token -- '-x'",
	} {
		_, err := deleteTokenCmd{dir: t.TempDir()}.Execute(t.Context(), tin{a: []string{name}, b: map[string]bool{"all": true}}, ui.Discard())
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, name)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		got := clierr.ExpandRemedies(ce.Message, "")
		assert.Contains(t, got, "to delete only "+clierr.ShellQuote(name)+", run: "+want+"; to delete every cached credential, run: rancher auth delete-token --all", name)
	}
}

// TestDeleteFailureTextPutsTheRerunLast asserts the message of a failed local
// delete says what failed and why before the command that finishes it, which
// ends the text.
func TestDeleteFailureTextPutsTheRerunLast(t *testing.T) {
	t.Parallel()
	text := deleteFailureText(`"prod"`, errors.New("the lock timed out"), clierr.NewCommand("auth delete-token").Arg("prod"))
	assert.Equal(t, `failed to delete "prod": the lock timed out; to finish, run: `+clierr.Rancher+" auth delete-token 'prod'", text)
}

// TestOwnerRefusalForAUserEntryJoinsTheRegenerateChoiceAfterTheLogin asserts
// the refusal of a call whose user entry holds another user's login ends the
// login command at a semicolon before it offers the regenerated kubeconfig,
// the same join a plugin login entry gets.
func TestOwnerRefusalForAUserEntryJoinsTheRegenerateChoiceAfterTheLogin(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: "https://r.example.com"}}}
	u := userCheck{userID: "u-abc12", clusterID: "c-1", serverID: "https://r.example.com"}
	for _, entry := range []string{"prod", "exec-login-r.ex"} {
		ce := u.entryOwnerRefused(cfg, entry, "u-other", entry != "prod", true)
		got := clierr.ExpandRemedies(ce.Message, "")
		assert.Regexp(t, `; or regenerate the kubeconfig from an entry that holds the login of 'u-abc12' with: rancher config use-server <entry>; then rancher kubeconfig --cluster 'c-1'`, got, entry)
		assert.NotContains(t, got, ", or regenerate", entry)
	}
	user := u.entryOwnerRefused(cfg, "prod", "u-other", false, true)
	assert.Contains(t, clierr.ExpandRemedies(user.Message, ""), "pipe an API token of 'u-abc12' to: rancher auth login 'prod' --token-stdin; or regenerate")
}

// TestKubeconfigRemedyNamesAPlaceholderForAnEmptyCluster asserts the command
// that writes a kubeconfig quotes a cluster id, writes a dashed id as a flag
// value, and names the given placeholder when there is no id, never an empty
// quoted value.
func TestKubeconfigRemedyNamesAPlaceholderForAnEmptyCluster(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "rancher kubeconfig --cluster 'c-1'", clierr.ExpandRemedies(kubeconfigRemedy("c-1").String(), ""))
	assert.Equal(t, "rancher kubeconfig --cluster '-c'", clierr.ExpandRemedies(kubeconfigRemedy("-c").String(), ""))
	assert.Equal(t, "rancher kubeconfig --cluster <id>", clierr.ExpandRemedies(kubeconfigRemedy("").String(), ""))
	assert.Equal(t, "rancher create kubeconfig --clusters <id> --apply --yes", clierr.ExpandRemedies(CreateKubeconfigRemedy("").String(), ""))
	assert.Equal(t, "rancher create kubeconfig --clusters 'c-1' --apply --yes", clierr.ExpandRemedies(CreateKubeconfigRemedy("c-1").String(), ""))
}

// TestNoTerminalLoginNamesTheCAFlagInsideTheCommand asserts the login command
// carries the CA the call resolved: a path as a quoted --cacert value, an
// inline PEM as the placeholder <file> with the holder said before the
// command, and nothing for an entry that has its own CA.
func TestNoTerminalLoginNamesTheCAFlagInsideTheCommand(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	const pem = "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n"
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: url}}}
	path := NoTerminalLogin(cfg, "team", "c-1", url, "/etc/rancher/ca.pem", config.CASource{}, false)
	assert.Equal(t, "rancher auth login 'team' --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin", clierr.ExpandRemedies(path.command.String(), ""))
	assert.Empty(t, path.Facts())
	assert.Equal(t, path.command, path.Hint())

	inline := NoTerminalLogin(cfg, "team", "c-1", url, pem, config.CASource{Kind: config.CASourceVariable}, false)
	assert.Equal(t, "rancher auth login 'team' --url 'https://r.example.com' --cacert <file> --token-stdin", clierr.ExpandRemedies(inline.command.String(), ""))
	assert.Equal(t, "RANCHER_CLI_CACERT carries the CA inline and --cacert needs a file; "+config.CAFileMeaning, inline.Facts())
	assert.Empty(t, inline.Hint(), "a command with a placeholder is no hint")
	assert.Equal(t, "rancher auth login 'team' --url 'https://r.example.com' --cacert <file> --auth-provider 'corp-ldap'", clierr.ExpandRemedies(inline.signIn("corp-ldap").String(), ""))

	own := NoTerminalLogin(cfg, "prod", "c-1", url, "/etc/rancher/ca.pem", config.CASource{}, false)
	assert.Equal(t, "rancher auth login 'prod' --url 'https://r.example.com' --token-stdin", clierr.ExpandRemedies(own.command.String(), ""))

	unnamed := NoTerminalLogin(cfg, "", "c-1", url, "", config.CASource{}, false)
	assert.Equal(t, "<name> stands for a name of your choice", unnamed.Facts())
	assert.Equal(t, "<name> stands for a name of your choice; pipe an API token to: "+unnamed.command.String(), unnamed.Instruction("pipe an API token"))
}

// TestLoginRemedyKeepsTheDashedNameAfterTheDoubleDash asserts the sign-in
// command of an entry whose name starts with a dash is built from the same
// parts as the piped login: its flags come before "--", which ends them.
func TestLoginRemedyKeepsTheDashedNameAfterTheDoubleDash(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	login := NoTerminalLogin(nil, "-x", "c-1", url, "", config.CASource{}, false)
	assert.Equal(t, "rancher auth login --url 'https://r.example.com' --token-stdin -- '-x'", clierr.ExpandRemedies(login.command.String(), ""))
	assert.Equal(t, "rancher auth login --url 'https://r.example.com' --auth-provider 'corp-ldap' -- '-x'", clierr.ExpandRemedies(login.signIn("corp-ldap").String(), ""))
}

// TestUseServerNoteSaysWhatTheSelectionChanges asserts the note after a
// command that selects an entry names the current server and the command that
// switches back, or says the directory has none.
func TestUseServerNoteSaysWhatTheSelectionChanges(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "; selecting an entry sets the directory's current server, which is unset now", useServerNote(""))
	assert.Equal(t, "; selecting an entry switches the directory's current server; to switch back, run: rancher config use-server 'prod'",
		clierr.ExpandRemedies(useServerNote("prod"), ""))
	assert.Equal(t, "rancher config use-server -- '-x'", clierr.ExpandRemedies(useServerCommand("-x").String(), ""))
	assert.Equal(t, "rancher config use-server <entry>", clierr.ExpandRemedies(useServerCommand("").String(), ""))
}

// TestTheRegenerateStepsRunTheSelectionThenTheKubeconfig asserts the steps
// kept in a kubeconfig file select the entry, write the kubeconfig and say
// what the selection does, with the opener the caller gives.
func TestTheRegenerateStepsRunTheSelectionThenTheKubeconfig(t *testing.T) {
	t.Parallel()
	got := RegenerateSteps(func(words clierr.CommandWord) clierr.Command { return clierr.FileCommandIn("/d", words) }, "prod", "c-1")
	assert.Equal(t, "rancher --config '/d' config use-server 'prod'; then rancher --config '/d' kubeconfig --cluster 'c-1'", got.String())
	assert.Equal(t, "rancher config use-server <entry>; then rancher kubeconfig --cluster <id>", RegenerateSteps(clierr.FileCommand, "", "").String())
	assert.Equal(t, "; selecting the entry switches the directory's current server", RegenerateNote)
}

// TestRevocationWarningNamesTheOwnerTheSequenceLogsInAs asserts the warning
// for a token that needs a manual revoke says the assumption before the
// commands, names the user the sequence logs in as in its own clause, and
// ends with the commands; and that the report carries the same user in its
// loginAs field, and none for a recovery that logs in as nobody.
func TestRevocationWarningNamesTheOwnerTheSequenceLogsInAs(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: url}}}
	ref := &TokenRef{Kind: TokenKindExt, Name: "token-abc"}
	d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{"prod": true}, req: LocalDelete{Rerun: clierr.NewCommand("auth logout").Arg("prod")}}
	it := &revocationItem{source: sourceCache, ref: ref, server: config.CanonicalServerIdentity(url), owner: "u-abc12", entry: "prod", reason: reasonFailed}
	warning := clierr.ExpandRemedies(d.manualWarning(it), "")
	assert.Contains(t, warning, "; the sequence "+clierr.RecoverySequenceAssumption+"; revoke it as user 'u-abc12' with: rancher auth login ")
	commands, loginAs := d.recovery(it)
	assert.Equal(t, "u-abc12", loginAs)
	assert.Len(t, commands, 4)

	noOwner := &revocationItem{source: sourceCache, ref: ref, server: config.CanonicalServerIdentity(url), entry: "prod", reason: reasonFailed}
	_, loginAs = d.recovery(noOwner)
	assert.Empty(t, loginAs)

	rerun := &revocationItem{source: sourceLogin, ref: ref, server: config.CanonicalServerIdentity(url), entry: "prod", reason: reasonNotDeleted}
	commands, loginAs = d.recovery(rerun)
	assert.Equal(t, []string{d.req.Rerun.Field()}, commands)
	assert.Empty(t, loginAs, "a rerun logs in as nobody")
	v3 := &revocationItem{source: sourceCache, ref: &TokenRef{Kind: TokenKindV3, Name: "t"}, server: config.CanonicalServerIdentity(url), reason: reasonV3}
	_, loginAs = d.recovery(v3)
	assert.Empty(t, loginAs, "an administrator's delete logs in as nobody")
}

// TestRevocationMessageKeepsTheMarkerThroughCertificateLines asserts the
// message of a certificate failure keeps the markers of the commands it holds
// through the line printer, and still removes every control character from
// the message and from the certificate's own server text.
func TestRevocationMessageKeepsTheMarkerThroughCertificateLines(t *testing.T) {
	t.Parallel()
	msg := "the certificate failed\x1b[31m; save the CA with: " + clierr.Rancher + " config update-server 'prod' --cacert <file>"
	cert := &tlstrust.Details{Kind: tlstrust.KindUnknownAuthority, Subject: "CN=evil\x1b[2Jhost", Fingerprint: "ab:cd"}
	lines := certificateFailureLines(msg, cert)
	require.GreaterOrEqual(t, len(lines), 2)
	assert.Equal(t, "the certificate failed[31m; save the CA with: "+clierr.Rancher+" config update-server 'prod' --cacert <file>", lines[0])
	for _, l := range lines {
		assert.NotContains(t, l, "\x1b")
	}
	assert.Contains(t, lines[1:], "certificate authority: CN=evil[2Jhost", "the certificate's own text lost its control characters")
}
