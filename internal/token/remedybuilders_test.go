package token

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
)

// remedyBuilders lists the functions of this package that write a remedy
// command, read from its source: every function whose body names
// clierr.Rancher or clierr.MarkRemedy, the two ways a remedy command is
// opened. A method is listed as Receiver.Method.
func remedyBuilders(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	var out []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, f, src, 0)
		require.NoError(t, err)
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			builds := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "clierr" && (sel.Sel.Name == "Rancher" || sel.Sel.Name == "MarkRemedy") {
						builds = true
					}
				}
				return !builds
			})
			if !builds {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				typ := fn.Recv.List[0].Type
				if star, ok := typ.(*ast.StarExpr); ok {
					typ = star.X
				}
				if id, ok := typ.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// errText is the message and hint of err, for a builder that returns an
// error.
func errText(err error) []string {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok {
		if err == nil {
			return nil
		}
		return []string{err.Error()}
	}
	return []string{ce.Message, ce.Hint}
}

// remedyBuilderRows is one row for each function that writes a remedy
// command: it runs the function on the inputs that reach each of its texts
// and returns them.
func remedyBuilderRows(t *testing.T) map[string]func() []string {
	t.Helper()
	const name = "exec-login-r.example.com"
	const url = "https://r.example.com"
	plugin := config.Server{URL: url, CA: "/etc/rancher/ca.pem", Provenance: clierr.ProvenanceExecLogin}
	// A plugin login entry's name on an entry recorded as a user entry: the
	// name alone decides what auth login refuses.
	userNamed := config.Server{URL: url}
	cfg := &config.Config{CurrentServer: name, Servers: map[string]config.Server{name: plugin, "prod": {URL: url}}}
	elsewhere := &config.Config{CurrentServer: name, Servers: map[string]config.Server{
		name: {URL: "https://other.example.com", Provenance: clierr.ProvenanceExecLogin}, "prod": {URL: url},
	}}
	u := userCheck{userID: "u-abc12", clusterID: "c-1", serverID: url, caSource: "/etc/rancher/ca.pem"}
	through := userCheck{userID: "u-abc12", authConfig: "corp-ldap", clusterID: "c-1", serverID: url, caSource: "/etc/rancher/ca.pem"}
	sc := storedCredential{name: name, entry: userNamed}
	ref := &TokenRef{Kind: TokenKindExt, Name: "token-abc"}
	created := CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: ref, UserID: "u-other"}
	fresh := &freshToken{create: created, cleanup: func(context.Context) RevokeOutcome { return RevokeOutcome{Result: ResultRevoked} }}
	dir := t.TempDir()

	rows := map[string]func() []string{
		"CreateKubeconfigRemedy": func() []string { return []string{CreateKubeconfigRemedy("c-1").String()} },
		"NoTerminalLogin": func() []string {
			var out []string
			for _, login := range []LoginRemedy{
				NoTerminalLogin(cfg, name, "c-1", url, "", config.CASource{}, false),
				NoTerminalLogin(cfg, name+"-c-1", "c-1", url, "", config.CASource{}, false),
				NoTerminalLogin(cfg, "prod", "c-1", url, "/etc/rancher/ca.pem", config.CASource{}, false),
				NoTerminalLogin(cfg, "", "c-1", url, "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n", config.CASource{}, false),
			} {
				out = append(out, login.Text(), login.Instruction("to log in, pipe an API token"), login.Hint().String(), login.signIn("corp-ldap").String())
			}
			return out
		},
		"RegenerateSteps": func() []string {
			return []string{RegenerateSteps(clierr.NewCommand, name, "c-1").String(), RegenerateSteps(clierr.NewCommand, "", "").String()}
		},
		"ResidualRecovery": func() []string {
			a, b := ResidualRecovery(clierr.TokenFate{Ref: ref}, name, "m", false)
			c, d := ResidualRecovery(clierr.TokenFate{Marked: true}, name, "m", false)
			e, f := ResidualRecovery(clierr.TokenFate{Ref: &TokenRef{Kind: TokenKindV3, Name: "token-v3"}}, name, "m", false)
			return []string{a, b.String(), c, d.String(), e, f.String()}
		},
		"signInLeftover": func() []string {
			var out []string
			for _, f := range signInLeftoverFates() {
				out = append(out, signInLeftover(f, "u-abc12", "m"), signInLeftover(f, "", "m"))
			}
			return out
		},
		"credentialRecoveryFor": func() []string {
			st := LoginStates{Commit: &config.CommitOutcome{Backend: clierr.BackendKeyring, State: config.CommitPossiblyCommitted}}
			return []string{credentialRecoveryFor(st, name).Command}
		},
		"entryNotNamedError": func() []string {
			return errText(entryNotNamedError(cfg, []storedCredential{{name: name, entry: userNamed}, {name: "prod", entry: cfg.Servers["prod"]}}, "c-1", false))
		},
		"entryOnOtherServerError": func() []string {
			return append(errText(entryOnOtherServerError(elsewhere, name, url, "c-1")),
				errText(entryOnOtherServerError(&config.Config{Servers: map[string]config.Server{name: elsewhere.Servers[name]}}, name, url, "c-1"))...)
		},
		"keptWarning": func() []string {
			return []string{
				keptWarning(config.SupersededToken{Ref: *ref, UserID: "u-abc12"}, url, "the server refused"),
				keptWarning(config.SupersededToken{Ref: *ref}, url, "the server refused"),
				keptWarning(config.SupersededToken{Ref: TokenRef{Kind: TokenKindV3, Name: "t"}, UserID: "u-abc12"}, url, "the server refused"),
			}
		},
		"namedEntryRoute": func() []string {
			route := func(name, cluster, current string) string {
				steps, _ := namedEntryRoute(name, cluster, current)
				return steps.String()
			}
			return []string{route(name, "c-1", "prod"), route("prod", "c-1", "staging"), route("prod", "", "prod"), namedEntryNote("prod", "staging")}
		},
		"Deletion.manualWarning": func() []string {
			d := &Deletion{cfg: cfg, view: cfg, req: LocalDelete{Rerun: clierr.NewCommand("auth logout").Arg(name)}}
			var out []string
			for _, it := range []*revocationItem{
				{source: sourceCache, ref: ref, server: url, owner: "u-abc12", entry: name, reason: reasonFailed},
				{source: sourceLogin, ref: ref, server: url, entry: name, reason: reasonNotDeleted},
				{source: sourceCache, server: url, reason: reasonUnproven},
				{source: sourceSuperseded, ref: &TokenRef{Kind: TokenKindV3, Name: "t"}, server: url, reason: reasonV3},
			} {
				out = append(out, d.manualWarning(it))
			}
			return out
		},
		"Deletion.revokeTLSRemedy": func() []string {
			owned := &config.Config{Servers: map[string]config.Server{name: {URL: url, TokenUserID: "u-abc12", Provenance: clierr.ProvenanceExecLogin}}}
			d := &Deletion{cfg: owned, view: owned, removed: map[string]bool{}, clients: map[string]revokeClient{}}
			server := config.CanonicalServerIdentity(url)
			v3 := &TokenRef{Kind: TokenKindV3, Name: "token-v3"}
			var out []string
			for _, it := range []*revocationItem{
				{source: sourceCache, ref: ref, server: server, owner: "u-abc12", entry: name, trust: name, reason: reasonFailed},
				{source: sourceCache, ref: ref, server: server, owner: "u-other", entry: name, trust: name, reason: reasonFailed},
				{source: sourceCache, ref: v3, server: server, owner: "u-abc12", entry: name, trust: name, reason: reasonFailed},
			} {
				for _, fromVariable := range []bool{false, true} {
					d.clients[name] = revokeClient{fromVariable: fromVariable}
					r := d.revokeTLSRemedy(it)
					out = append(out, r.CA, r.URL)
				}
			}
			return out
		},
		"Deletion.Warnings": func() []string {
			owned := &config.Config{Servers: map[string]config.Server{name: {URL: url, TokenUserID: "u-abc12", Provenance: clierr.ProvenanceExecLogin}}}
			d := &Deletion{cfg: owned, view: owned, removed: map[string]bool{}, clients: map[string]revokeClient{name: {}}}
			d.items = []*revocationItem{{source: sourceCache, ref: ref, server: config.CanonicalServerIdentity(url), owner: "u-abc12", entry: name, trust: name,
				reason: reasonFailed, tlsErr: clierr.New(clierr.CodeTLSTrust, "x"), cert: &tlstrust.Details{Kind: tlstrust.KindUnknownAuthority}}}
			out := d.Warnings()
			userOwned := &config.Config{Servers: map[string]config.Server{"prod": {URL: url, TokenUserID: "u-abc12"}}}
			d = &Deletion{cfg: userOwned, view: userOwned, removed: map[string]bool{}, clients: map[string]revokeClient{"prod": {}}}
			d.items = []*revocationItem{{source: sourceCache, ref: ref, server: config.CanonicalServerIdentity(url), owner: "u-abc12", entry: "prod", trust: "prod",
				reason: reasonFailed, tlsErr: clierr.New(clierr.CodeTLSTrust, "x"), cert: &tlstrust.Details{Kind: tlstrust.KindUnknownAuthority}}}
			return append(out, d.Warnings()...)
		},
		"deleteTokenCmd.Execute": func() []string {
			res, err := deleteTokenCmd{dir: dir, store: config.NewFileStore(dir)}.Execute(t.Context(), tin{a: []string{name}}, ui.Discard())
			require.NoError(t, err)
			return append([]string{res.Text}, res.Warnings...)
		},
		"Deletion.nextLoginText": func() []string {
			named := &config.Config{Servers: map[string]config.Server{name: userNamed}}
			d := &Deletion{cfg: named, view: named}
			userOwned := &config.Config{Servers: map[string]config.Server{"prod": {URL: url, TokenUserID: "u-abc12"}}}
			return []string{d.nextLoginText(name, true, clierr.IDText), (&Deletion{cfg: cfg, view: cfg}).nextLoginText(name, true, clierr.IDText),
				(&Deletion{cfg: userOwned, view: userOwned}).nextLoginText("prod", true, clierr.IDText)}
		},
		"v3Remedy": func() []string {
			ref := TokenRef{Kind: TokenKindV3, Name: "token-v3"}
			return []string{v3Remedy(ref, "2026-10-06T00:00:00Z"), v3Remedy(ref, "")}
		},
		"v3AdminRemedy": func() []string {
			ref := TokenRef{Kind: TokenKindV3, Name: "token-v3"}
			return []string{v3AdminRemedy(ref, "2026-10-06T00:00:00Z"), v3AdminRemedy(ref, "")}
		},
		"v3AdminDelete": func() []string {
			return []string{v3AdminDelete("it", TokenRef{Kind: TokenKindV3, Name: "token-v3"}, "")}
		},
		"pluginEntriesError": func() []string {
			names := []string{name, name + "-2"}
			return append(errText(pluginEntriesError(url, names, cfg, "", userCheck{})),
				append(errText(pluginEntriesError(url, names, cfg, "prod", u)), errText(pluginEntriesError(url, names, cfg, "", u))...)...)
		},
		"noTokenPermissionTextOf": func() []string {
			return []string{noTokenPermissionText("u-abc12", true), noTokenPermissionText("", true), noTokenPermissionText("u-abc12", false), noTokenPermissionText("", false),
				noTokenPermissionTextOf("alice@example.com", name, true), noTokenPermissionTextOf("u-a\u202eb", "", false)}
		},
		"reLogin":          func() []string { a, b := reLogin(name); return []string{a, b.String()} },
		"revokedClause":    func() []string { return []string{revokedClause("the token", ref, RevokeOutcome{Result: ResultFailed})} },
		"useServerCommand": func() []string { return []string{useServerCommand(name).String(), useServerCommand("").String()} },
		"useServerNote":    func() []string { return []string{useServerNote("prod"), useServerNote("")} },
		"LoginRemedy.signIn": func() []string {
			var out []string
			for _, login := range []LoginRemedy{
				NoTerminalLogin(nil, "team", "c-1", url, "", config.CASource{}, false),
				NoTerminalLogin(nil, "team", "c-1", url, "/etc/rancher/ca.pem", config.CASource{}, false),
				NoTerminalLogin(nil, "team", "c-1", url, "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n", config.CASource{}, false),
				NoTerminalLogin(nil, "", "c-1", url, "", config.CASource{}, false),
				NoTerminalLogin(nil, "", "c-1", url, "/etc/rancher/ca.pem", config.CASource{}, false),
				NoTerminalLogin(nil, "", "c-1", url, "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n", config.CASource{}, false),
			} {
				out = append(out, login.signIn("corp-ldap").String())
			}
			return out
		},
		"LoginRemedy.pipedLogin": func() []string {
			var out []string
			for _, login := range []LoginRemedy{
				NoTerminalLogin(nil, "team", "c-1", url, "", config.CASource{}, false),
				NoTerminalLogin(nil, "-x", "c-1", url, "/etc/rancher/ca.pem", config.CASource{}, false),
				NoTerminalLogin(nil, "", "c-1", url, "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n", config.CASource{}, false),
			} {
				out = append(out, login.pipedLogin().String())
			}
			return out
		},
		"kubeconfigRemedy": func() []string { return []string{kubeconfigRemedy("c-1").String(), kubeconfigRemedy("").String()} },
		"deleteFailureText": func() []string {
			return []string{deleteFailureText(`"prod"`, errors.New("the lock timed out"), clierr.NewCommand("auth delete-token").Arg("prod"))}
		},
		"Deletion.recovery": func() []string {
			d := &Deletion{cfg: cfg, view: cfg, req: LocalDelete{Rerun: clierr.NewCommand("auth logout").Arg(name)}}
			var out []string
			for _, it := range []*revocationItem{
				{source: sourceCache, ref: ref, server: url, owner: "u-abc12", entry: name, reason: reasonFailed},
				{source: sourceLogin, ref: ref, server: url, entry: name, reason: reasonNotDeleted},
				{source: sourceCache, server: url, reason: reasonUnproven},
				{source: sourceSuperseded, ref: &TokenRef{Kind: TokenKindV3, Name: "t"}, server: url, reason: reasonV3},
			} {
				commands, _ := d.recovery(it)
				out = append(out, commands...)
			}
			return out
		},
		"CreateKubeconfigRemedyEmpty": func() []string { return []string{CreateKubeconfigRemedy("").String()} },
		"userCheck.relogin": func() []string {
			a, b := u.relogin(cfg, name, true)
			c, d := u.relogin(cfg, "prod", false)
			e, f := u.relogin(cfg, name, false)
			g, h := through.relogin(cfg, name, true)
			i, j := through.relogin(cfg, "prod", false)
			return []string{a, b.String(), c, d.String(), e, f.String(), g, h.String(), i, j.String()}
		},
		"userCheck.entryNameTooLong": func() []string {
			return append(errText(u.entryNameTooLong(cfg, config.MaxEntryNameLength+1)), errText(through.entryNameTooLong(cfg, config.MaxEntryNameLength+1))...)
		},
		"userCheck.authConfigRemedy": func() []string {
			a, b := through.authConfigRemedy(cfg, name, true)
			c, d := through.authConfigRemedy(cfg, "prod", false)
			e, f := through.authConfigRemedy(cfg, name, false)
			return []string{a, b.String(), c, d.String(), e, f.String()}
		},
		"userCheck.loginOfNewEntry": func() []string { return []string{u.loginOfNewEntry(cfg)} },
		"userCheck.entryOwnerRefused": func() []string {
			return append(errText(u.entryOwnerRefused(cfg, name, "u-other", false, true)), errText(u.entryOwnerRefused(cfg, name, "u-other", true, false))...)
		},
		"userCheck.checkCreatedOwner": func() []string {
			var out []string
			for _, c := range []storedCredential{sc, {name: name, entry: plugin}, {name: "prod", entry: userNamed}} {
				out = append(out, errText(u.checkCreatedOwner(t.Context(), cfg, created, fresh, c))...)
			}
			return out
		},
		"tokenCmd.classifyRejection": func() []string {
			var notices bytes.Buffer
			_ = tokenCmd{}.classifyRejection(errCredentialUnauthorized, sc, "", true, ui.New(&notices, nil, true, false))
			return []string{notices.String()}
		},
		"tokenCmd.noTerminalError": func() []string {
			call := noTerminalCall{name: name + "-c-1", clusterID: "c-1", serverURL: url, userID: "u-abc12"}
			choice := clierr.New(clierr.CodeAmbiguousSelection, "x").WithCause(&providerChoiceNeedsTerminal{
				configs: []string{"local", "okta"}, noTerminal: []clierr.Candidate{{ID: "okta", Name: "okta"}}})
			return append(errText(tokenCmd{dir: dir}.withNoTerminalRemedies(interactiveLoginRequired("local", false), call)),
				errText(tokenCmd{dir: dir}.withNoTerminalRemedies(choice, call))...)
		},
		"tokenCmd.withStoreConflictSteps": func() []string {
			conflict := clierr.New(clierr.CodeConflict, "conflict").WithCause(config.ErrCredentialStoreConflict)
			return errText(tokenCmd{dir: dir}.withStoreConflictSteps(conflict, url, name+"-c-1", "c-1"))
		},
	}
	return rows
}

// TestNoRemedyBuilderNamesAuthLoginOnAPluginLoginEntry asserts no remedy
// this package writes runs auth login on a plugin login entry, whose name
// auth login refuses: every builder, found in the source, is run with a
// plugin login entry's name wherever it takes an entry name, and its text
// is searched. A builder added without a row here fails the test.
func TestNoRemedyBuilderNamesAuthLoginOnAPluginLoginEntry(t *testing.T) {
	rows := remedyBuilderRows(t)

	// Every function that opens a remedy command has a row. The rows beyond
	// them run functions that only join such text (checkCreatedOwner), or
	// take the opener from their caller (RegenerateSteps).
	builders := remedyBuilders(t)
	for _, b := range builders {
		assert.Contains(t, rows, b, "remedy builder %s has no row", b)
	}
	t.Logf("remedy builders found in the source: %s", strings.Join(builders, ", "))
	loginOnPlugin := regexp.MustCompile(`auth login '?` + regexp.QuoteMeta(config.ExecLoginPrefix))
	for builder, run := range rows {
		for _, text := range run() {
			assert.NotRegexp(t, loginOnPlugin, clierr.ExpandRemedies(text, ""), builder)
			assertCommandsEndTheirSentence(t, clierr.ExpandRemedies(text, " --config '/d'"))
		}
	}
}

// endCommandRE finds the start of a remedy command in text an invocation
// with a config directory printed: rancher and its --config.
var endCommandRE = regexp.MustCompile(`rancher --config '[^']*'`)

// endWordRE is the next word of a command, as the operator reads it: a
// quoted value, a flag, a lowercase word, a placeholder or a URL. A word
// that ends in a dot or a comma ends before it; the next character is the
// one that follows the command.
var endWordRE = regexp.MustCompile(`^(?:'(?:[^']|'\\'')*'|--?[a-z][a-z-]*|--|[a-z0-9]+(?:[.-][a-z0-9]+)*|<[a-z-]+>|https?://[^\s,;)'` + "`" + `]+)`)

// endStopWords are the words of running text that may follow a command
// glued to it, and that the command does not take as an argument.
var endStopWords = []string{"again", "first", "once", "then", "and", "or", "to", "for", "with", "if", "which", "as", "until", "after", "before", "now", "instead"}

// assertCommandsEndTheirSentence asserts every remedy command in text, which
// an invocation with a config directory printed, ends its clause: the
// character after the command is the end of the text, a semicolon, a line
// break, or a closing quote or backtick. A command is cut at the first word
// of endStopWords: a word of running text glued after it is seen; a
// lowercase word that is not in the list is read as an argument of the
// command and is not seen.
func assertCommandsEndTheirSentence(t *testing.T, text string) {
	t.Helper()
	rest := text
	for {
		loc := endCommandRE.FindStringIndex(rest)
		if loc == nil {
			return
		}
		pos := loc[1]
		for pos < len(rest) && rest[pos] == ' ' {
			word := endWordRE.FindString(rest[pos+1:])
			if word == "" || slices.Contains(endStopWords, word) {
				break
			}
			pos += 1 + len(word)
		}
		if pos < len(rest) {
			assert.Contains(t, ";\n'`", string(rest[pos]), "the command ends its clause: %q", rest[loc[0]:min(len(rest), pos+12)])
		}
		rest = rest[pos:]
	}
}

// TestAPluginLoginEntryAtAnotherURLGetsANewEntryLogin asserts the refusal
// of a call whose --name identifies a plugin login entry registered at
// another URL: the route to that URL logs in a new entry there, with the
// plugin login entry's CA - for one held inline, naming that entry's
// config.yaml record as where it is - never auth login on the plugin
// login entry; the
// route to the call's URL, when an entry is registered there, is as for a
// user entry.
func TestAPluginLoginEntryAtAnotherURLGetsANewEntryLogin(t *testing.T) {
	t.Parallel()
	const other = "https://other.example.com"
	const pemCA = "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n"
	newEntry := func(ca string) string {
		login := "<name> stands for a name of your choice; pipe an API token to: rancher auth login <name> --url '" + other + "' --cacert '/etc/rancher/other-ca.pem' --token-stdin"
		if ca == pemCA {
			// The plugin-created login records the CA inline; the exec entry
			// carries the call's server's CA, not this one.
			login = "<name> stands for a name of your choice; server entry 'exec-login-o' carries the CA inline in config.yaml and --cacert needs a file; " + config.CAFileMeaning + "; " +
				"pipe an API token to: rancher auth login <name> --url '" + other + "' --cacert <file> --token-stdin"
		}
		return "log in there under a server entry of your own, since auth login refuses the exec plugin's own login entries; " + login +
			"; then rancher config use-server <name>; then rancher kubeconfig --cluster 'c-1'"
	}
	const switchNote = "; selecting an entry switches the directory's current server; to switch back, run: rancher config use-server 'prod'"
	for _, tc := range []struct {
		name string
		here bool
		ca   string
		want string
	}{
		{"no entry at the call's URL", false, "/etc/rancher/other-ca.pem", "the exec entry names server entry 'exec-login-o', which is registered at " + other +
			", but this call runs for https://r.example.com and no entry is registered there; " + newEntry("/etc/rancher/other-ca.pem") + switchNote},
		{"no entry at the call's URL, the CA inline", false, pemCA, "the exec entry names server entry 'exec-login-o', which is registered at " + other +
			", but this call runs for https://r.example.com and no entry is registered there; " + newEntry(pemCA) + switchNote},
		{"an entry at the call's URL", true, "/etc/rancher/other-ca.pem", "the exec entry names server entry 'exec-login-o', which is registered at " + other +
			", but this call runs for https://r.example.com; to use 'exec-login-o', " + newEntry("/etc/rancher/other-ca.pem") +
			"; to use https://r.example.com, regenerate it from an entry registered there with: rancher config use-server <entry>; then rancher kubeconfig --cluster 'c-1'" + switchNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{
				"prod":         {URL: "https://prod.example.com"},
				"exec-login-o": {URL: other, CA: tc.ca, Provenance: clierr.ProvenanceExecLogin},
			}}
			if tc.here {
				cfg.Servers["staging"] = config.Server{URL: "https://r.example.com"}
			}
			_, err := identifyEntry(cfg, "https://r.example.com", "exec-login-o-c-1", "c-1", false)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeConflict, ce.Code)
			assert.Equal(t, tc.want, clierr.ExpandRemedies(ce.Message, ""))
		})
	}
}

// TestResidualRecoveryOfASAMLTokenTheCLICannotNameIsTheSignInText asserts the
// whole text for a kubeconfig token the CLI cannot name, with and without the
// request's expiry: the window clause, the facts, the administrator's command
// that lists the candidates, opened by clierr.Rancher, and the UI route for the
// user who signed in. It is the text the exec plugin's sign-in gives.
func TestResidualRecoveryOfASAMLTokenTheCLICannotNameIsTheSignInText(t *testing.T) {
	t.Parallel()
	const head = "a kubeconfig token may have been created through saml at or after 2026-10-12T10:00:00Z"
	const tail = "; the CLI stopped waiting at 2026-10-12T10:05:00Z, " +
		"but the browser sign-in can still complete after that, and the token may be newer; " +
		"these facts narrow the candidates but cannot identify one token; " +
		"an administrator can inspect them with: "
	const command = " get management-tokens -o json; the user who signed in can find them in the UI under Account & API Keys"
	search := clierr.UnmarkedTokenSearch{AuthProvider: "saml", HandoffAt: "2026-10-12T10:00:00Z", PollEndedAt: "2026-10-12T10:05:00Z"}
	text, hint := ResidualRecovery(clierr.TokenFate{Search: &search}, "prod", "m", true)
	assert.Equal(t, head+tail+clierr.Rancher+command, text)
	assert.Empty(t, hint)
	search.RequestExpiresAt = "2026-10-12T10:10:00Z"
	text, _ = ResidualRecovery(clierr.TokenFate{Search: &search}, "prod", "m", true)
	assert.Equal(t, head+" and not after 2026-10-12T10:10:00Z"+tail+clierr.Rancher+command, text)
}
