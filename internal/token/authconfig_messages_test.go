package token

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// messageRow is one message a builder writes: its text and its hint, with
// every remedy written for the default directory.
type messageRow struct{ name, message, hint string }

// authMessageRows builds each message the checks of a call with --user-id
// write, for a call with the auth config authConfig, or none when it is
// empty: the refusals that name the user, the ones that name the auth
// config (written only with one), the length refusal, the several-entries
// refusal and the no-terminal refusal.
func authMessageRows(t *testing.T, authConfig string) []messageRow {
	t.Helper()
	const url = "https://r.example.com"
	plugin := config.Server{URL: url, CA: "/etc/rancher/ca.pem", Provenance: clierr.ProvenanceExecLogin}
	user := config.Server{URL: url}
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"exec-login-r.example.com": plugin, "prod": user}}
	ref := &TokenRef{Kind: TokenKindExt, Name: "token-abc"}
	created := CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: ref, UserID: "u-other", AuthConfig: "openldap"}
	fresh := &freshToken{create: created, cleanup: func(context.Context) RevokeOutcome { return RevokeOutcome{Result: ResultRevoked} }}
	u := userCheck{userID: "u-abc12", authConfig: authConfig, clusterID: "c-1", serverID: url, caSource: "/etc/rancher/ca.pem"}
	var rows []messageRow
	row := func(name string, err error) {
		t.Helper()
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, name)
		rows = append(rows, messageRow{name, clierr.ExpandRemedies(ce.Message, ""), clierr.ExpandRemedies(ce.Hint, "")})
	}
	text, hint := u.relogin(cfg, "prod", false)
	rows = append(rows, messageRow{"the login of a user entry", clierr.ExpandRemedies(text, ""), clierr.ExpandRemedies(hint, "")})
	text, hint = u.relogin(cfg, "exec-login-r.example.com", true)
	rows = append(rows, messageRow{"the removal of a plugin login entry", clierr.ExpandRemedies(text, ""), clierr.ExpandRemedies(hint, "")})
	row("another user's login in a user entry", u.entryOwnerRefused(cfg, "prod", "u-other", false, true))
	row("another user's login in a plugin login entry", u.entryOwnerRefused(cfg, "exec-login-r.example.com", "u-other", true, true))
	row("a plugin login entry last logged in as another user", u.entryOwnerRefused(cfg, "exec-login-r.example.com", "u-other", true, false))
	row("a created token of another user, from a user entry", u.checkCreatedOwner(context.Background(), cfg, created, fresh, storedCredential{name: "prod", entry: user}))
	row("a created token of another user, from a plugin login entry", u.checkCreatedOwner(context.Background(), cfg, created, fresh, storedCredential{name: "exec-login-r.example.com", entry: plugin}))
	row("a plugin login entry name too long for the store", u.entryNameTooLong(cfg, 500))
	if authConfig != "" {
		row("a login through another auth config in a user entry", u.entryAuthConfigRefused(cfg, "prod", "openldap", false, true))
		row("a login through another auth config in a plugin login entry", u.entryAuthConfigRefused(cfg, "exec-login-r.example.com", "openldap", true, true))
		row("a plugin login entry last logged in through another auth config", u.entryAuthConfigRefused(cfg, "exec-login-r.example.com", "openldap", true, false))
		created.UserID = "u-abc12"
		row("a created token through another auth config, from a user entry", u.refuseOtherAuthConfig(context.Background(), cfg, created, fresh, storedCredential{name: "prod", entry: user}))
		row("a created token through another auth config, from a plugin login entry", u.refuseOtherAuthConfig(context.Background(), cfg, created, fresh, storedCredential{name: "exec-login-r.example.com", entry: plugin}))
		row("a created token that names no auth config", u.refuseNoAuthConfig(context.Background(), created, fresh, storedCredential{name: "prod", entry: user}))
		row("the plugin's sign-in through another auth config", u.pluginAuthConfigRefused("openldap"))
		row("the plugin's sign-in tokens name no auth config", u.pluginAuthConfigRefused(""))
	}
	names := []string{"exec-login-r.example.com", "exec-login-r.example.com-2"}
	row("several plugin login entries for an identified entry", pluginEntriesError(url, names, cfg, "prod", u))
	row("several plugin login entries for no entry", pluginEntriesError(url, names, cfg, "", u))
	for _, c := range []struct {
		label string
		call  noTerminalCall
	}{
		{"no terminal, a named entry", noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: url, userID: "u-abc12", authConfig: authConfig}},
		{"no terminal, no entry to log in to", noTerminalCall{clusterID: "c-1", serverURL: url, userID: "u-abc12", authConfig: authConfig, keepsLogin: true}},
	} {
		row(c.label, tokenCmd{dir: t.TempDir()}.withNoTerminalRemedies(interactiveLoginRequired("local", "x"), c.call))
	}
	return disambiguate(rows)
}

// disambiguate numbers the rows that share a name.
func disambiguate(rows []messageRow) []messageRow {
	seen := map[string]int{}
	for i, r := range rows {
		seen[r.name]++
		if seen[r.name] > 1 {
			rows[i].name = r.name + " " + string(rune('0'+seen[r.name]))
		}
	}
	return rows
}

// TestMessagesWithoutAnAuthConfigAreUnchanged asserts each message the
// owner checks write for a call with --user-id and no auth config is the
// text the checks had before an auth config existed: the user entry's and
// the plugin login entry's refusals of another owner, of a created token of
// another owner, the length refusal, the several-entries refusals and the
// no-terminal refusals, one row each.
func TestMessagesWithoutAnAuthConfigAreUnchanged(t *testing.T) {
	t.Parallel()
	want := []messageRow{
		{"the login of a user entry", "pipe an API token of 'u-abc12' to rancher auth login 'prod' --token-stdin", "rancher auth login 'prod' --token-stdin"},
		{"the removal of a plugin login entry", "remove 'exec-login-r.example.com' with rancher auth logout 'exec-login-r.example.com', then rancher config delete-server 'exec-login-r.example.com'; the next call signs in as 'u-abc12' in a terminal, or uses a login piped to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>", ""},
		{"another user's login in a user entry", "server entry 'prod' holds the login of Rancher user 'u-other', and this exec entry names user 'u-abc12' with --user-id; pipe an API token of 'u-abc12' to rancher auth login 'prod' --token-stdin, or regenerate the kubeconfig from an entry that holds the login of 'u-abc12' with rancher config use-server <entry> (this switches the directory's current server; switch back with rancher config use-server 'prod'), then rancher kubeconfig --cluster 'c-1'", "rancher auth login 'prod' --token-stdin"},
		{"another user's login in a plugin login entry", "server entry 'exec-login-r.example.com', the exec plugin's own login entry, holds the login of Rancher user 'u-other', and this exec entry names user 'u-abc12' with --user-id; remove 'exec-login-r.example.com' with rancher auth logout 'exec-login-r.example.com', then rancher config delete-server 'exec-login-r.example.com'; the next call signs in as 'u-abc12' in a terminal, or uses a login piped to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>; or regenerate the kubeconfig from an entry that holds the login of 'u-abc12' with rancher config use-server <entry> (this switches the directory's current server; switch back with rancher config use-server 'prod'), then rancher kubeconfig --cluster 'c-1'", ""},
		{"a plugin login entry last logged in as another user", "server entry 'exec-login-r.example.com', the exec plugin's own login entry, was last logged in as Rancher user 'u-other', and this exec entry names user 'u-abc12' with --user-id; remove 'exec-login-r.example.com' with rancher auth logout 'exec-login-r.example.com', then rancher config delete-server 'exec-login-r.example.com'; the next call signs in as 'u-abc12' in a terminal, or uses a login piped to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>; or regenerate the kubeconfig from an entry that holds the login of 'u-abc12' with rancher config use-server <entry> (this switches the directory's current server; switch back with rancher config use-server 'prod'), then rancher kubeconfig --cluster 'c-1'", ""},
		{"a created token of another user, from a user entry", "the stored login of server entry 'prod' belongs to Rancher user 'u-other', not to 'u-abc12', which this exec entry names with --user-id; the token created from it was revoked; pipe an API token of 'u-abc12' to rancher auth login 'prod' --token-stdin", "rancher auth login 'prod' --token-stdin"},
		{"a created token of another user, from a plugin login entry", "the stored login of server entry 'exec-login-r.example.com' belongs to Rancher user 'u-other', not to 'u-abc12', which this exec entry names with --user-id; the token created from it was revoked; remove 'exec-login-r.example.com' with rancher auth logout 'exec-login-r.example.com', then rancher config delete-server 'exec-login-r.example.com'; the next call signs in as 'u-abc12' in a terminal, or uses a login piped to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>", ""},
		{"a plugin login entry name too long for the store", "the exec plugin's login entry for this server would have a name of 500 characters, and the credential store accepts at most 467; to log in under a shorter name, pipe an API token of 'u-abc12' to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>", ""},
		{"several plugin login entries for an identified entry", "more than one login entry the exec plugin created answers for https://r.example.com ('exec-login-r.example.com', 'exec-login-r.example.com-2'); pipe an API token of 'u-abc12' to rancher auth login 'prod' --token-stdin to log in to 'prod'; or keep one: for each other entry run rancher auth logout <entry>, then rancher config delete-server <entry>", "rancher auth login 'prod' --token-stdin"},
		{"several plugin login entries for no entry", "more than one login entry the exec plugin created answers for https://r.example.com ('exec-login-r.example.com', 'exec-login-r.example.com-2'); pipe an API token of 'u-abc12' to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin to log in an entry for 'https://r.example.com', with a name of your choice for <name>; or keep one: for each other entry run rancher auth logout <entry>, then rancher config delete-server <entry>", ""},
		{"no terminal, a named entry", "interactive login required for auth config local; this call has no terminal to answer it; to log in without one, pipe an API token of Rancher user 'u-abc12' to rancher auth login 'prod' --url 'https://r.example.com' --token-stdin; or run kubectl once in a terminal and sign in as 'u-abc12', which lasts until the cached token expires", "rancher auth login 'prod' --url 'https://r.example.com' --token-stdin"},
		{"no terminal, no entry to log in to", "interactive login required for auth config local; this call has no terminal to answer it; to log in without one, pipe an API token of Rancher user 'u-abc12' to rancher auth login <name> --url 'https://r.example.com' --token-stdin, with a name of your choice for <name>; or run kubectl once in a terminal and sign in as 'u-abc12'", ""},
	}
	assert.Equal(t, want, authMessageRows(t, ""))
}

// TestMessagesWithAnAuthConfigNameIt asserts each message for a call with
// --user-id and an auth config: the owner checks' refusals name it in the
// login they ask for, the refusals of an entry or a token of another auth
// config name both and the way to a login through the call's, the sign-in
// refusals say nothing was stored, and every remedy is valid as written.
func TestMessagesWithAnAuthConfigNameIt(t *testing.T) {
	t.Parallel()
	want := []messageRow{
		{"the login of a user entry", "pipe an API token of 'u-abc12' made through auth config 'corp-ldap' to rancher auth login 'prod' --token-stdin", "rancher auth login 'prod' --token-stdin"},
		{"the removal of a plugin login entry", "remove 'exec-login-r.example.com' with rancher auth logout 'exec-login-r.example.com', then rancher config delete-server 'exec-login-r.example.com'; the next call signs in as 'u-abc12' through auth config 'corp-ldap' in a terminal, or uses a login made through auth config 'corp-ldap' and piped to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>", ""},
		{"another user's login in a user entry", "server entry 'prod' holds the login of Rancher user 'u-other', and this exec entry names user 'u-abc12' with --user-id; pipe an API token of 'u-abc12' made through auth config 'corp-ldap' to rancher auth login 'prod' --token-stdin, or regenerate the kubeconfig from an entry that holds the login of 'u-abc12' with rancher config use-server <entry> (this switches the directory's current server; switch back with rancher config use-server 'prod'), then rancher kubeconfig --cluster 'c-1'", "rancher auth login 'prod' --token-stdin"},
		{"another user's login in a plugin login entry", "server entry 'exec-login-r.example.com', the exec plugin's own login entry, holds the login of Rancher user 'u-other', and this exec entry names user 'u-abc12' with --user-id; remove 'exec-login-r.example.com' with rancher auth logout 'exec-login-r.example.com', then rancher config delete-server 'exec-login-r.example.com'; the next call signs in as 'u-abc12' through auth config 'corp-ldap' in a terminal, or uses a login made through auth config 'corp-ldap' and piped to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>; or regenerate the kubeconfig from an entry that holds the login of 'u-abc12' with rancher config use-server <entry> (this switches the directory's current server; switch back with rancher config use-server 'prod'), then rancher kubeconfig --cluster 'c-1'", ""},
		{"a plugin login entry last logged in as another user", "server entry 'exec-login-r.example.com', the exec plugin's own login entry, was last logged in as Rancher user 'u-other', and this exec entry names user 'u-abc12' with --user-id; remove 'exec-login-r.example.com' with rancher auth logout 'exec-login-r.example.com', then rancher config delete-server 'exec-login-r.example.com'; the next call signs in as 'u-abc12' through auth config 'corp-ldap' in a terminal, or uses a login made through auth config 'corp-ldap' and piped to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>; or regenerate the kubeconfig from an entry that holds the login of 'u-abc12' with rancher config use-server <entry> (this switches the directory's current server; switch back with rancher config use-server 'prod'), then rancher kubeconfig --cluster 'c-1'", ""},
		{"a created token of another user, from a user entry", "the stored login of server entry 'prod' belongs to Rancher user 'u-other', not to 'u-abc12', which this exec entry names with --user-id; the token created from it was revoked; pipe an API token of 'u-abc12' made through auth config 'corp-ldap' to rancher auth login 'prod' --token-stdin", "rancher auth login 'prod' --token-stdin"},
		{"a created token of another user, from a plugin login entry", "the stored login of server entry 'exec-login-r.example.com' belongs to Rancher user 'u-other', not to 'u-abc12', which this exec entry names with --user-id; the token created from it was revoked; remove 'exec-login-r.example.com' with rancher auth logout 'exec-login-r.example.com', then rancher config delete-server 'exec-login-r.example.com'; the next call signs in as 'u-abc12' through auth config 'corp-ldap' in a terminal, or uses a login made through auth config 'corp-ldap' and piped to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>", ""},
		{"a plugin login entry name too long for the store", "the exec plugin's login entry for this server, for Rancher user 'u-abc12' through auth config 'corp-ldap', would have a name of 500 characters, and the credential store accepts at most 467; to log in under a shorter name, sign in with rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --auth-provider 'corp-ldap', or pipe an API token of 'u-abc12' made through auth config 'corp-ldap' to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>", ""},
		{"a login through another auth config in a user entry", "server entry 'prod' holds a login made through auth config 'openldap', and this exec entry names --auth-provider 'corp-ldap'; sign in through auth config 'corp-ldap' with rancher auth login 'prod' --auth-provider 'corp-ldap', or pipe an API token of 'u-abc12' made through auth config 'corp-ldap' to rancher auth login 'prod' --token-stdin", "rancher auth login 'prod' --auth-provider 'corp-ldap'"},
		{"a login through another auth config in a plugin login entry", "server entry 'exec-login-r.example.com', the exec plugin's own login entry, holds a login made through auth config 'openldap', and this exec entry names --auth-provider 'corp-ldap'; remove 'exec-login-r.example.com' with rancher auth logout 'exec-login-r.example.com', then rancher config delete-server 'exec-login-r.example.com'; the next call signs in as 'u-abc12' through auth config 'corp-ldap' in a terminal, or uses a login made through auth config 'corp-ldap' and piped to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>", ""},
		{"a plugin login entry last logged in through another auth config", "server entry 'exec-login-r.example.com', the exec plugin's own login entry, was last logged in through auth config 'openldap', and this exec entry names --auth-provider 'corp-ldap'; remove 'exec-login-r.example.com' with rancher auth logout 'exec-login-r.example.com', then rancher config delete-server 'exec-login-r.example.com'; the next call signs in as 'u-abc12' through auth config 'corp-ldap' in a terminal, or uses a login made through auth config 'corp-ldap' and piped to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>", ""},
		{"a created token through another auth config, from a user entry", "the token created from the stored login of server entry 'prod' was made through auth config 'openldap', not 'corp-ldap', which this exec entry names with --auth-provider; the token was revoked; sign in through auth config 'corp-ldap' with rancher auth login 'prod' --auth-provider 'corp-ldap', or pipe an API token of 'u-abc12' made through auth config 'corp-ldap' to rancher auth login 'prod' --token-stdin", "rancher auth login 'prod' --auth-provider 'corp-ldap'"},
		{"a created token through another auth config, from a plugin login entry", "the token created from the stored login of server entry 'exec-login-r.example.com' was made through auth config 'openldap', not 'corp-ldap', which this exec entry names with --auth-provider; the token was revoked; remove 'exec-login-r.example.com' with rancher auth logout 'exec-login-r.example.com', then rancher config delete-server 'exec-login-r.example.com'; the next call signs in as 'u-abc12' through auth config 'corp-ldap' in a terminal, or uses a login made through auth config 'corp-ldap' and piped to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin, with a name of your choice for <name>", ""},
		{"a created token that names no auth config", "the token create response for server entry 'prod' named no auth config; it cannot be checked against --auth-provider 'corp-ldap'; the token was revoked", ""},
		{"the plugin's sign-in through another auth config", "the sign-in was made through auth config 'openldap', not 'corp-ldap', which this exec entry names with --auth-provider; nothing was stored; on the sign-in page, choose auth config 'corp-ldap'", ""},
		{"the plugin's sign-in tokens name no auth config", "the tokens the sign-in created named no auth config; they cannot be checked against --auth-provider 'corp-ldap'; nothing was stored", ""},
		{"several plugin login entries for an identified entry", "more than one login entry the exec plugin created answers for https://r.example.com ('exec-login-r.example.com', 'exec-login-r.example.com-2'); pipe an API token of 'u-abc12' made through auth config 'corp-ldap' to rancher auth login 'prod' --token-stdin to log in to 'prod'; or keep one: for each other entry run rancher auth logout <entry>, then rancher config delete-server <entry>", "rancher auth login 'prod' --token-stdin"},
		{"several plugin login entries for no entry", "more than one login entry the exec plugin created answers for https://r.example.com ('exec-login-r.example.com', 'exec-login-r.example.com-2'); pipe an API token of 'u-abc12' made through auth config 'corp-ldap' to rancher auth login <name> --url 'https://r.example.com' --cacert '/etc/rancher/ca.pem' --token-stdin to log in an entry for 'https://r.example.com', with a name of your choice for <name>; or keep one: for each other entry run rancher auth logout <entry>, then rancher config delete-server <entry>", ""},
		{"no terminal, a named entry", "interactive login required for auth config local; this call has no terminal to answer it; to log in without one, pipe an API token of Rancher user 'u-abc12' made through auth config 'corp-ldap' to rancher auth login 'prod' --url 'https://r.example.com' --token-stdin; or run kubectl once in a terminal and sign in as 'u-abc12' through auth config 'corp-ldap', which lasts until the cached token expires", "rancher auth login 'prod' --url 'https://r.example.com' --token-stdin"},
		{"no terminal, no entry to log in to", "interactive login required for auth config local; this call has no terminal to answer it; to log in without one, pipe an API token of Rancher user 'u-abc12' made through auth config 'corp-ldap' to rancher auth login <name> --url 'https://r.example.com' --token-stdin, with a name of your choice for <name>; or run kubectl once in a terminal and sign in as 'u-abc12' through auth config 'corp-ldap'", ""},
	}
	assert.Equal(t, want, authMessageRows(t, "corp-ldap"))
}
