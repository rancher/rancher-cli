package token

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// assertLeftOutClause asserts text, once the remedy marker is expanded, holds
// every fact in present, none of the words in absent, ends with the note about
// the left-out command, holds the note once, and has no lead-in word before
// the note.
func assertLeftOutClause(t *testing.T, text string, present, absent []string) {
	t.Helper()
	text = clierr.ExpandRemedies(text, "")
	assert.Equal(t, 1, strings.Count(text, clierr.LeftOutNote), text)
	require.True(t, strings.HasSuffix(text, "; "+clierr.LeftOutNote), text)
	for _, fact := range present {
		assert.Contains(t, text, clierr.ExpandRemedies(fact, ""))
	}
	for _, word := range absent {
		assert.NotContains(t, text, word)
	}
	before := strings.TrimSuffix(text, "; "+clierr.LeftOutNote)
	for _, dangling := range []string{":", ";", " with", " to", " run", " of", " or", " then"} {
		assert.False(t, strings.HasSuffix(before, dangling), "a lead-in word %q is left before the note: %s", dangling, text)
	}
	assert.NotContains(t, text, "; ;")
	assert.NotContains(t, text, ": ;")
	assert.NotContains(t, text, ":;")
}

// nextLoginDeletion is a delete plan with one token that the next login of
// the entry revokes: the entry records the token's owner at the token's
// server. cert is the certificate failure of the token's revoke, or nil.
func nextLoginDeletion(entry string, cert *tlstrust.Details) (*Deletion, *revocationItem) {
	const url = "https://r.example.com"
	cfg := &config.Config{Servers: map[string]config.Server{entry: {URL: url, TokenUserID: "u-abc12"}}}
	ref := &TokenRef{Kind: TokenKindExt, Name: "token-abc12"}
	it := &revocationItem{source: sourceCache, ref: ref, server: config.CanonicalServerIdentity(url), owner: "u-abc12",
		entry: entry, trust: entry, reason: reasonFailed, cert: cert}
	d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{}, items: []*revocationItem{it}}
	return d, it
}

// TestTheCertificateRemedyOfARevokeLeavesOutTheSaveForAnEntryThatCannotBePrinted
// asserts the CA clause of a certificate failure of a revoke that the next
// login of an entry retries keeps the words that name the entry and what
// <file> stands for, and leaves out the config update-server command and its
// lead-in, whether the CA came from the variable or not, with the note once.
func TestTheCertificateRemedyOfARevokeLeavesOutTheSaveForAnEntryThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedNames {
		for _, fromVariable := range []bool{false, true} {
			name := tc.name + ", the CA from a file"
			if fromVariable {
				name = tc.name + ", the CA from the variable"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				d, it := nextLoginDeletion(tc.value, nil)
				d.clients = map[string]revokeClient{tc.value: {fromVariable: fromVariable}}
				require.Equal(t, retryNextLogin, d.retry(it))
				remedy := d.revokeTLSRemedy(it)
				assertLeftOutClause(t, remedy.CA,
					[]string{config.CAFileMeaning, "entry " + clierr.ShellQuote(tc.value)},
					[]string{"update-server", ", run:", " with: "})
			})
		}
	}
}

// TestTheWarningOfTokensThatTheNextLoginRevokesLeavesOutTheCommandsForAnEntryThatCannotBePrinted
// asserts the warning line of an entry whose next login revokes its tokens
// keeps the count, the entry and the words about the certificate when the
// entry name cannot be printed, leaving out the auth login and, for a
// certificate failure, the config update-server commands, with the note once.
func TestTheWarningOfTokensThatTheNextLoginRevokesLeavesOutTheCommandsForAnEntryThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedNames {
		t.Run(tc.name+", without a certificate failure", func(t *testing.T) {
			t.Parallel()
			d, _ := nextLoginDeletion(tc.value, nil)
			warnings := d.Warnings()
			require.Len(t, warnings, 1)
			assertLeftOutClause(t, warnings[0],
				[]string{"1 token(s) recorded on " + tc.value + " are revoked by the next login of " + tc.value + " as u-abc12"},
				[]string{"auth login", "run it with"})
		})
		t.Run(tc.name+", with a certificate failure", func(t *testing.T) {
			t.Parallel()
			d, _ := nextLoginDeletion(tc.value, &tlstrust.Details{Kind: tlstrust.KindUnknownAuthority})
			warnings := d.Warnings()
			require.Len(t, warnings, 1)
			assertLeftOutClause(t, warnings[0],
				[]string{"1 token(s) recorded on " + tc.value + " are revoked by the next login of " + tc.value, "its revoke failed on the server's certificate", "first save the server's CA"},
				[]string{"auth login", "update-server", "run it with", " with: "})
		})
	}
}

// TestTheNextLoginTextOfAnEntryThatCannotBePrintedLeavesOutTheLogin asserts the
// text that names the login that revokes an entry's tokens keeps the words
// that name the entry and the user and leaves out the command and its
// lead-in, with the note once, and that it names the command for a plain
// entry name.
func TestTheNextLoginTextOfAnEntryThatCannotBePrintedLeavesOutTheLogin(t *testing.T) {
	t.Parallel()
	plain, _ := nextLoginDeletion("prod", nil)
	assert.Equal(t, "the next login of prod as u-abc12; run it with: "+clierr.Rancher+" auth login 'prod'", plain.nextLoginText("prod", true, clierr.IDText))
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := nextLoginDeletion(tc.value, nil)
			assertLeftOutClause(t, d.nextLoginText(tc.value, true, clierr.IDText),
				[]string{"the next login of " + tc.value + " as u-abc12"},
				[]string{"auth login", "run it with"})
		})
	}
}

// TestTheRemovalOfAPluginLoginEntryThatCannotBePrintedIsLeftOutOfTheRelogin
// asserts the relogin text of a plugin login entry keeps the sentence that
// names the entry and what the next call does when the entry name cannot be
// printed, leaves out both steps of the removal and the lead-in, gives no
// hint, and holds the note once.
func TestTheRemovalOfAPluginLoginEntryThatCannotBePrintedIsLeftOutOfTheRelogin(t *testing.T) {
	t.Parallel()
	u := userCheck{userID: "u-abc12", clusterID: "c-1", serverID: "https://r.example.com"}
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			text, hint := u.relogin(&config.Config{}, tc.value, true)
			assert.False(t, hint.OK() && hint.Field() != "")
			assertLeftOutClause(t, text,
				[]string{"remove " + clierr.ShellQuote(tc.value), "the next call signs in as 'u-abc12' in a terminal"},
				[]string{"auth logout", "delete-server", "remove " + clierr.ShellQuote(tc.value) + " with"})
		})
	}
}

// TestTheSignInOfAnAuthConfigRemedyIsLeftOutForAValueThatCannotBePrinted
// asserts the remedy of a login made through another auth config keeps the
// words about the auth config and the piped login, leaves out the sign-in
// command and its lead-in and gives no hint, whether the entry name or the
// auth config cannot be printed, with the note once.
func TestTheSignInOfAnAuthConfigRemedyIsLeftOutForAValueThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedNames {
		t.Run(tc.name+", the entry", func(t *testing.T) {
			t.Parallel()
			u := userCheck{userID: "u-abc12", authConfig: "corp-ldap", clusterID: "c-1", serverID: "https://r.example.com"}
			text, hint := u.authConfigRemedy(&config.Config{}, tc.value, false)
			assert.Empty(t, hint.Field())
			assertLeftOutClause(t, text,
				[]string{"sign in through auth config 'corp-ldap'", "pipe an API token of 'u-abc12' made through auth config 'corp-ldap'"},
				[]string{"--auth-provider", "auth login", "sign in through auth config 'corp-ldap' with"})
		})
		t.Run(tc.name+", the auth config", func(t *testing.T) {
			t.Parallel()
			u := userCheck{userID: "u-abc12", authConfig: tc.value, clusterID: "c-1", serverID: "https://r.example.com"}
			text, hint := u.authConfigRemedy(&config.Config{}, "prod", false)
			assert.Empty(t, hint.Field())
			assertLeftOutClause(t, text,
				[]string{"sign in through auth config " + clierr.ShellQuote(tc.value), "; or pipe an API token of 'u-abc12' made through auth config " + clierr.ShellQuote(tc.value) + " to: " + clierr.Rancher + " auth login 'prod' --token-stdin"},
				[]string{"--auth-provider"})
		})
	}
}

// TestTheCreateKubeconfigClauseOfTheNoTerminalRefusalIsLeftOutForAClusterThatCannotBePrinted
// asserts the no-terminal refusal of a call whose current login serves it
// keeps the login and the sign-in remedies and drops the clause that offers
// create kubeconfig when the cluster id cannot be printed, with the note
// once, and keeps the clause for a plain cluster id.
func TestTheCreateKubeconfigClauseOfTheNoTerminalRefusalIsLeftOutForAClusterThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	refuse := func(t *testing.T, clusterID string) string {
		t.Helper()
		dir := t.TempDir()
		cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: url}}}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		store := config.NewFileStore(dir)
		require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: url}, "id:secret"))
		err := tokenCmd{dir: dir, store: store}.withNoTerminalRemedies(interactiveLoginRequired("local", false), noTerminalCall{name: "prod-c-1", clusterID: clusterID, serverURL: url})
		_, msg := refusal(t, err)
		return msg
	}
	assert.Contains(t, refuse(t, "c-1"), "; for a kubeconfig that carries its own token, run: rancher create kubeconfig --clusters 'c-1' --apply --yes")
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertLeftOutClause(t, refuse(t, tc.value),
				[]string{"to log in without a terminal, pipe an API token", "or run kubectl once in a terminal and sign in, which lasts until the cached token expires"},
				[]string{"create kubeconfig", "--clusters"})
		})
	}
}

// TestTheSignInOfALoginRemedyIsLeftOutForAnAuthConfigThatCannotBePrinted
// asserts the sentence that offers a sign-in through an auth config, and the
// piped login after it, keeps the sign-in prose and the piped login and leaves
// out the sign-in command and its lead-in for an auth config that cannot be
// printed, with the note once.
func TestTheSignInOfALoginRemedyIsLeftOutForAnAuthConfigThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	login := NoTerminalLogin(nil, "prod", "c-1", "https://r.example.com", "", config.CASource{}, false)
	plain := login.InstructionSignIn("to sign in in a terminal", "corp-ldap", "to pipe a token")
	assert.Equal(t, "to sign in in a terminal with: "+clierr.Rancher+" auth login 'prod' --url 'https://r.example.com' --auth-provider 'corp-ldap'; or to pipe a token to: "+
		clierr.Rancher+" auth login 'prod' --url 'https://r.example.com' --token-stdin", plain)
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			text := login.InstructionSignIn("to sign in in a terminal", tc.value, "to pipe a token")
			assertLeftOutClause(t, text,
				[]string{"to sign in in a terminal", "; or to pipe a token to: " + clierr.Rancher + " auth login 'prod' --url 'https://r.example.com' --token-stdin"},
				[]string{"--auth-provider", "to sign in in a terminal with"})
		})
	}
}

// TestTheLoginOfAnEntryNameThatCannotBePrintedIsLeftOutOfTheNoTerminalInstruction
// asserts the instruction to pipe an API token keeps its prose and gives no
// hint when the entry name the login names cannot be printed, with the note
// once.
func TestTheLoginOfAnEntryNameThatCannotBePrintedIsLeftOutOfTheNoTerminalInstruction(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			login := NoTerminalLogin(nil, tc.value, "c-1", "https://r.example.com", "", config.CASource{}, false)
			require.True(t, login.Named)
			assert.Empty(t, login.Hint().Field())
			assertLeftOutClause(t, login.Instruction("to log in, pipe an API token"),
				[]string{"to log in, pipe an API token"},
				[]string{"auth login", "pipe an API token to"})
			assertLeftOutClause(t, login.InstructionSignIn("to sign in", "corp-ldap", "to pipe a token"),
				[]string{"to sign in", "; or to pipe a token"},
				[]string{"auth login", "--auth-provider", "to sign in with"})
		})
	}
}

// TestTheCACommandOfALoginThatCannotBePrintedIsLeftOutOfTheNoTerminalInstruction
// asserts the login that names a CA file keeps the prose and leaves out the
// whole login, with the note once, when the path of the CA file cannot be
// printed, and prints it for a plain path.
func TestTheCACommandOfALoginThatCannotBePrintedIsLeftOutOfTheNoTerminalInstruction(t *testing.T) {
	t.Parallel()
	plain := NoTerminalLogin(nil, "prod", "c-1", "https://r.example.com", "/etc/ca.pem", config.CASource{}, false)
	assert.Equal(t, "to log in, pipe an API token to: "+clierr.Rancher+" auth login 'prod' --url 'https://r.example.com' --cacert '/etc/ca.pem' --token-stdin",
		plain.Instruction("to log in, pipe an API token"))
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			login := NoTerminalLogin(nil, "prod", "c-1", "https://r.example.com", "/etc/"+tc.value+".pem", config.CASource{}, false)
			assert.Empty(t, login.Hint().Field())
			assertLeftOutClause(t, login.Instruction("to log in, pipe an API token"),
				[]string{"to log in, pipe an API token"},
				[]string{"auth login", "--cacert", "pipe an API token to"})
		})
	}
}

// TestTheSwitchBackCommandOfACurrentServerThatCannotBePrintedIsLeftOut asserts
// the note about selecting an entry keeps its sentence and leaves out the
// command that switches back, with its lead-in and the note once, when the
// current server's name cannot be printed, in the note itself and in the
// refusal of several entries that hold a stored login.
func TestTheSwitchBackCommandOfACurrentServerThatCannotBePrintedIsLeftOut(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "; selecting an entry switches the directory's current server; to switch back, run: "+clierr.Rancher+" config use-server 'prod'", useServerNote("prod"))
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			note := useServerNote(tc.value)
			assert.True(t, strings.HasPrefix(note, "; selecting an entry switches the directory's current server"), note)
			assert.Equal(t, 1, strings.Count(note, clierr.LeftOutNote), note)
			assert.NotContains(t, note, "to switch back")
			assert.NotContains(t, note, "use-server")
			assert.True(t, strings.HasSuffix(note, "; "+clierr.LeftOutNote), note)

			cfg := &config.Config{CurrentServer: tc.value, Servers: map[string]config.Server{
				"a": {URL: "https://r.example.com"}, "b": {URL: "https://r.example.com"},
			}}
			hits := []storedCredential{{name: "a", entry: cfg.Servers["a"]}, {name: "b", entry: cfg.Servers["b"]}}
			ce := entryNotNamedError(cfg, hits, "c-1", false)
			assertLeftOutClause(t, ce.Message,
				[]string{"more than one server entry", "selecting an entry switches the directory's current server", "regenerate the kubeconfig from the entry you want with: rancher config use-server <entry>; then rancher kubeconfig --cluster 'c-1'"},
				[]string{"to switch back", "use-server " + clierr.ShellQuote(tc.value)})
		})
	}
}

// TestTheLoginOfTheRouteToANamedEntryThatCannotBePrintedIsLeftOutWhenItIsTheCurrentServer
// asserts the refusal of a call that names an entry registered at another URL
// keeps its facts and leaves the route out, with its lead-in and the note
// once, when the entry is the current server and its name cannot be printed,
// whether or not an entry is registered at the call's URL.
func TestTheLoginOfTheRouteToANamedEntryThatCannotBePrintedIsLeftOutWhenItIsTheCurrentServer(t *testing.T) {
	t.Parallel()
	const url, other = "https://r.example.com", "https://other.example.com"
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			steps, ok := namedEntryRoute(tc.value, "c-1", tc.value)
			require.True(t, ok)
			assert.False(t, steps.OK())
			assert.Nil(t, steps.Lines())

			quoted := clierr.ShellQuote(tc.value)
			noEntryHere := &config.Config{CurrentServer: tc.value, Servers: map[string]config.Server{tc.value: {URL: other}}}
			ce := entryOnOtherServerError(noEntryHere, tc.value, url, "c-1")
			assertLeftOutClause(t, ce.Message,
				[]string{"the exec entry names server entry " + quoted, "but this call runs for " + url + " and no entry is registered there", "regenerate the kubeconfig from " + quoted},
				[]string{"auth login", "kubeconfig --cluster", "regenerate the kubeconfig from " + quoted + ":"})

			entryHere := &config.Config{CurrentServer: tc.value, Servers: map[string]config.Server{tc.value: {URL: other}, "prod": {URL: url}}}
			ce = entryOnOtherServerError(entryHere, tc.value, url, "c-1")
			assertLeftOutClause(t, ce.Message,
				[]string{"to use " + quoted + ", regenerate the kubeconfig from it", "to use " + url + ", regenerate it from an entry registered there"},
				[]string{"auth login", "from it:"})
		})
	}
}
