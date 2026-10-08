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

// TestNoRemedyBuilderNamesAuthLoginOnAPluginLoginEntry asserts no remedy
// this package writes runs auth login on a plugin login entry, whose name
// auth login refuses: every builder, found in the source, is run with a
// plugin login entry's name wherever it takes an entry name, and its text
// is searched. A builder added without a row here fails the test.
func TestNoRemedyBuilderNamesAuthLoginOnAPluginLoginEntry(t *testing.T) {
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
	sc := storedCredential{name: name, entry: userNamed}
	ref := &TokenRef{Kind: TokenKindExt, Name: "token-abc"}
	created := CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: ref, UserID: "u-other"}
	fresh := &freshToken{create: created, cleanup: func(context.Context) RevokeOutcome { return RevokeOutcome{Result: ResultRevoked} }}
	dir := t.TempDir()

	rows := map[string]func() []string{
		"CreateKubeconfigRemedy": func() []string { return []string{CreateKubeconfigRemedy("c-1")} },
		"NoTerminalLogin": func() []string {
			return []string{NoTerminalLogin(cfg, name, "c-1", url, "", false).Text(), NoTerminalLogin(cfg, name+"-c-1", "c-1", url, "", false).Text()}
		},
		"RegenerateSteps": func() []string { return []string{RegenerateSteps(clierr.Rancher, name, "c-1")} },
		"ResidualRecovery": func() []string {
			a, b := ResidualRecovery(clierr.TokenFate{Ref: ref}, name, "m", false)
			c, d := ResidualRecovery(clierr.TokenFate{Marked: true}, name, "m", false)
			return []string{a, b, c, d}
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
			return []string{keptWarning(config.SupersededToken{Ref: *ref, UserID: "u-abc12"}, url, "the server refused")}
		},
		"namedEntryRoute": func() []string { return []string{namedEntryRoute(name, clierr.ShellQuote(name), "'c-1'", "prod")} },
		"Deletion.manualWarning": func() []string {
			d := &Deletion{cfg: cfg, view: cfg, req: LocalDelete{Rerun: clierr.Rancher + " auth logout " + clierr.ShellQuote(name)}}
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
		"deleteTokenCmd.Execute": func() []string {
			res, err := deleteTokenCmd{dir: dir, store: config.NewFileStore(dir)}.Execute(t.Context(), tin{a: []string{name}}, ui.Discard())
			require.NoError(t, err)
			return append([]string{res.Text}, res.Warnings...)
		},
		"Deletion.nextLoginText": func() []string {
			named := &config.Config{Servers: map[string]config.Server{name: userNamed}}
			d := &Deletion{cfg: named, view: named}
			return []string{d.nextLoginText(name, true), (&Deletion{cfg: cfg, view: cfg}).nextLoginText(name, true)}
		},
		"v3Remedy": func() []string {
			return []string{v3Remedy(TokenRef{Kind: TokenKindV3, Name: "token-v3"}, "2026-10-06T00:00:00Z")}
		},
		"pluginEntriesError": func() []string { return errText(pluginEntriesError(url, []string{name, name + "-2"}, cfg)) },
		"reLogin":            func() []string { a, b := reLogin(name); return []string{a, b} },
		"readUserID": func() []string {
			_, err := readUserID(tin{s: map[string]string{"user-id": "u-abc12"}}, "", "c-1")
			return errText(err)
		},
		"revokedClause": func() []string { return []string{revokedClause("the token", ref, RevokeOutcome{Result: ResultFailed})} },
		"unmarkedSessionText": func() []string {
			return []string{unmarkedSessionText(clierr.UnmarkedTokenSearch{AuthProvider: "saml"})}
		},
		"useServerRemedy":           func() []string { return []string{useServerRemedy(clierr.ShellQuote(name), "prod")} },
		"userCheck.loginOfEntry":    func() []string { a, b := u.loginOfEntry(cfg, name); return []string{a, b} },
		"userCheck.loginOfNewEntry": func() []string { return []string{u.loginOfNewEntry(cfg)} },
		"userCheck.entryOwnerRefused": func() []string {
			return append(errText(u.entryOwnerRefused(cfg, name, "u-other", false, true)), errText(u.entryOwnerRefused(cfg, name, "u-other", true, false))...)
		},
		"userCheck.checkCreatedOwner": func() []string {
			var out []string
			for _, c := range []struct {
				sc         storedCredential
				identified string
			}{{sc, name}, {sc, ""}, {storedCredential{name: name, entry: plugin}, "prod"}, {storedCredential{name: "prod", entry: userNamed}, name}} {
				out = append(out, errText(u.checkCreatedOwner(t.Context(), cfg, created, fresh, c.sc, c.identified))...)
			}
			return out
		},
		"tokenCmd.classifyRejection": func() []string {
			var notices bytes.Buffer
			_ = tokenCmd{}.classifyRejection(errCredentialUnauthorized, sc, ui.New(&notices, nil, true, false))
			return []string{notices.String()}
		},
		"tokenCmd.withNoTerminalRemedies": func() []string {
			return errText(tokenCmd{dir: dir}.withNoTerminalRemedies(interactiveLoginRequired("localProvider", "x"),
				noTerminalCall{name: name + "-c-1", clusterID: "c-1", serverURL: url, userID: "u-abc12", skipped: name, skippedOwner: "u-other"}))
		},
		"tokenCmd.withNewEntryLogin": func() []string {
			_, err := readUserID(tin{s: map[string]string{"user-id": "u-abc12"}}, "", "c-1")
			return errText(tokenCmd{dir: dir}.withNewEntryLogin(err, tin{s: map[string]string{"server": url, "cluster": "c-1", "user-id": "u-abc12"}}))
		},
		"tokenCmd.withStoreConflictSteps": func() []string {
			conflict := clierr.New(clierr.CodeConflict, "conflict").WithCause(config.ErrCredentialStoreConflict)
			return errText(tokenCmd{dir: dir}.withStoreConflictSteps(conflict, url, name+"-c-1", "c-1"))
		},
	}

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
		}
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
		login := "pipe an API token to rancher auth login <name> --url '" + other + "' --cacert '/etc/rancher/other-ca.pem' --token-stdin, with a name of your choice for <name>, "
		if ca == pemCA {
			// The plugin-created login records the CA inline; the exec entry
			// carries the call's server's CA, not this one.
			login = "pipe an API token to rancher auth login <name> --url '" + other + "' --token-stdin, with a name of your choice for <name>, " +
				"adding --cacert with the path of a file that holds the server's CA certificate, which server entry 'exec-login-o' carries inline in config.yaml, "
		}
		return "log in there under a server entry of your own, since auth login refuses the exec plugin's own login entries: " + login +
			"then rancher config use-server <name> (this switches the directory's current server; switch back with rancher config use-server 'prod'), then rancher kubeconfig --cluster 'c-1'"
	}
	for _, tc := range []struct {
		name string
		here bool
		ca   string
		want string
	}{
		{"no entry at the call's URL", false, "/etc/rancher/other-ca.pem", "the exec entry names server entry 'exec-login-o', which is registered at " + other +
			", but this call runs for https://r.example.com and no entry is registered there; " + newEntry("/etc/rancher/other-ca.pem")},
		{"no entry at the call's URL, the CA inline", false, pemCA, "the exec entry names server entry 'exec-login-o', which is registered at " + other +
			", but this call runs for https://r.example.com and no entry is registered there; " + newEntry(pemCA)},
		{"an entry at the call's URL", true, "/etc/rancher/other-ca.pem", "the exec entry names server entry 'exec-login-o', which is registered at " + other +
			", but this call runs for https://r.example.com; to use 'exec-login-o', " + newEntry("/etc/rancher/other-ca.pem") +
			"; to use https://r.example.com, regenerate it from an entry registered there with rancher config use-server <entry> (this switches the directory's current server; switch back with rancher config use-server 'prod'), then rancher kubeconfig --cluster 'c-1'"},
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
