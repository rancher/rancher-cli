package token

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// grantTail is the end of the administrator's command in the permission
	// text, after the user's id.
	grantTail = " --global --role user --dry-run"
	// permissionLead opens the permission text of every entry.
	permissionLead = "the user has no role that allows creating tokens (create on tokens.ext.cattle.io); "
)

// TestNoTokenPermissionTextNamesTheGrant asserts the text with a known id
// quotes the id in the command, and without one says how to find the id and
// writes <user-id> in the command, with no other placeholder; neither text
// carries a hint-shaped second command.
func TestNoTokenPermissionTextNamesTheGrant(t *testing.T) {
	t.Parallel()
	const grant = "an administrator can grant the global role user: add --apply --yes after reading the preview of: "
	assert.Equal(t, permissionLead+grant+clierr.Rancher+" grant 'u-abc12'"+grantTail, noTokenPermissionText("u-abc12", true))
	assert.Equal(t, permissionLead+"find the user's id with: "+clierr.Rancher+" get users; "+grant+clierr.Rancher+" grant <user-id>"+grantTail, noTokenPermissionText("", true))
	assert.Equal(t, permissionLead+grant+clierr.Rancher+" grant 'a b'"+grantTail, noTokenPermissionText("a b", true), "an id is one quoted argument")
	assert.True(t, strings.HasSuffix(noTokenPermissionText("-x", true), grantTail+" -- '-x'"), "an id that starts with a dash follows --")
}

// forbiddenAcquire runs a login whose derived token create is answered 403
// and returns the failure FailAcquire reports for the server entry name.
func forbiddenAcquire(t *testing.T, opts testrancher.Options, server string) (*clierr.Error, clierr.LoginFailureDetails) {
	t.Helper()
	opts.DerivedCreateStatus = http.StatusForbidden
	_, _, _, err := acquireAgainst(t, opts)
	ae, ok := errors.AsType[*AcquireError](err)
	require.True(t, ok, "got %v", err)
	return failureOf(t, FailAcquire(ae, server))
}

// TestFailAcquireForbiddenNamesThePermission asserts a login refused the
// permission to create a token says the user has no role that allows it, the
// administrator's grant command for the user's id (or <user-id> when the
// login named none), and then how to log in again, once; the error carries no
// hint when every token is deleted.
func TestFailAcquireForbiddenNamesThePermission(t *testing.T) {
	t.Parallel()
	pluginEntry := config.ExecLoginPrefix + "r.example.com"
	loginAgain, _ := reLogin("prod")
	pluginAgain, _ := reLogin(pluginEntry)

	t.Run("no user id", func(t *testing.T) {
		t.Parallel()
		ce, d := forbiddenAcquire(t, testrancher.Options{}, "prod")
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Equal(t, noTokenPermissionText("", true)+"; "+loginAgain, d.Recovery)
		assert.Empty(t, ce.Hint)
	})

	t.Run("user id named by the login", func(t *testing.T) {
		t.Parallel()
		_, d := forbiddenAcquire(t, testrancher.Options{LegacyLoginBodyUserID: true}, "prod")
		assert.Equal(t, noTokenPermissionText("user-e2e", true)+"; "+loginAgain, d.Recovery)
		assert.Contains(t, d.Recovery, clierr.Rancher+" grant 'user-e2e'"+grantTail)
	})

	t.Run("plugin login entry", func(t *testing.T) {
		t.Parallel()
		ce, d := forbiddenAcquire(t, testrancher.Options{}, pluginEntry)
		assert.Equal(t, noTokenPermissionText("", true)+"; "+pluginAgain, d.Recovery)
		assert.Contains(t, d.Recovery, "run the kubectl command again")
		assert.NotContains(t, d.Recovery, "auth login")
		assert.Empty(t, ce.Hint)
	})

	t.Run("session token left behind", func(t *testing.T) {
		t.Parallel()
		ce, d := forbiddenAcquire(t, testrancher.Options{SessionDeleteMode: "fail"}, "prod")
		residual, _ := ResidualRecovery(*d.SessionToken, "prod", d.OperationMarker, false)
		require.NotEmpty(t, residual)
		assert.Equal(t, noTokenPermissionText("", true)+"; "+residual, d.Recovery, "the permission text first, then the recovery for the token")
		assert.Equal(t, 1, strings.Count(d.Recovery, loginAgain), "how to log in again is said once")
		assert.Equal(t, loginAgain[len("log in again with: "):], ce.Hint, "the hint stays the login command")
	})

	t.Run("remedy commands expand with the caller's directory", func(t *testing.T) {
		t.Parallel()
		_, d := forbiddenAcquire(t, testrancher.Options{LegacyLoginBodyUserID: true}, "prod")
		assert.Contains(t, clierr.ExpandRemedies(d.Recovery, ""), "rancher grant 'user-e2e'"+grantTail)
		assert.Contains(t, clierr.ExpandRemedies(d.Recovery, " --config '/etc/r'"), "rancher --config '/etc/r' grant 'user-e2e'"+grantTail)
		assert.Contains(t, clierr.ExpandRemedies(d.Recovery, " --config '/etc/r'"), "rancher --config '/etc/r' auth login 'prod'")
	})
}

// TestSignInForbiddenEndsWithTheKubectlCommandAgain asserts the sign-in of the
// exec plugin refused the permission to create a token for Rancher itself,
// over ext and over Norman, says the user has no role that allows it and ends
// with signInAgain, never with the login command. The ext text names the
// ext token resource, the Norman text names none, and the id is the user the
// login named when the refused create names none.
func TestSignInForbiddenEndsWithTheKubectlCommandAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		opts   testrancher.Options
		ext    bool
		userID string
	}{
		{"ext", testrancher.Options{DerivedCreateStatus: http.StatusForbidden}, true, ""},
		{"ext with the user the login named", testrancher.Options{DerivedCreateStatus: http.StatusForbidden, LegacyLoginBodyUserID: true}, true, "user-e2e"},
		{"Norman", testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateStatus: http.StatusForbidden}, false, ""},
		{"Norman with the user the login named", testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateStatus: http.StatusForbidden, LegacyLoginBodyUserID: true}, false, "user-e2e"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, _, err := signInAgainst(t, tc.opts, "")
			ce, d := failureOf(t, err)
			assert.Equal(t, clierr.CodeRBAC, ce.Code)
			assert.Equal(t, noTokenPermissionText(tc.userID, tc.ext)+"; "+signInAgain, d.Recovery)
			assert.Equal(t, tc.ext, strings.Contains(d.Recovery, "tokens.ext.cattle.io"))
			assert.NotContains(t, d.Recovery, "auth login")
			assert.Empty(t, ce.Hint)
		})
	}
}

// storedLoginFixture is a stored login of a user entry, whose token the test
// Rancher accepts as a bearer, with the entry recording userID as its user.
// opts forces the status of the token creates the stored login makes;
// provenance says whether a user or the exec plugin made the login.
func storedLoginFixture(t *testing.T, opts testrancher.Options, userID string, provenance clierr.Provenance) *pluginFixture {
	t.Helper()
	opts.SeedToken = "seed-token:secret"
	x := newPluginFixture(t, true, opts)
	seedEntry(t, x.dir, x.store, "prod", config.Server{URL: x.url, Provenance: provenance, TokenUserID: userID}, opts.SeedToken, true)
	return x
}

// getTokenNoCluster is get-token for a token for Rancher itself: the entry
// name and the user id it records, and no cluster.
func (x *pluginFixture) getTokenNoCluster(t *testing.T, name, userID string) (runtime.Result, error) {
	t.Helper()
	in := tin{s: map[string]string{"server": x.url, "name": name, "user-id": userID}}
	return x.command(x.store).Execute(t.Context(), in, localPrompter())
}

// TestStoredLoginForbiddenHasThePermissionTextAndNoLoginLine asserts a
// get-token run whose stored login may not create a token for Rancher itself
// names the missing role and the grant for the entry's recorded user id, and
// says nothing about logging in: the stored login stays valid.
func TestStoredLoginForbiddenHasThePermissionTextAndNoLoginLine(t *testing.T) {
	x := storedLoginFixture(t, testrancher.Options{DerivedCreateStatus: http.StatusForbidden}, "user-e2e", clierr.ProvenanceDerived)
	_, err := x.getTokenNoCluster(t, "prod", "user-e2e")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.True(t, strings.HasSuffix(ce.Message, "; "+noTokenPermissionText("user-e2e", true)), ce.Message)
	assert.NotContains(t, ce.Message, "log in")
	assert.Empty(t, ce.Hint)
}

// TestPluginStoredLoginOverNormanForbiddenNamesNoExtResource asserts a stored
// plugin login with no cluster, on a Rancher without ext.cattle.io whose
// Norman token create answers 403, gets the permission text without the ext
// token resource.
func TestPluginStoredLoginOverNormanForbiddenNamesNoExtResource(t *testing.T) {
	ce := pluginStoredLoginOverNormanRefusal(t)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.True(t, strings.HasSuffix(ce.Message, "; "+noTokenPermissionText("user-e2e", false)), ce.Message)
	assert.NotContains(t, ce.Message, "tokens.ext.cattle.io")
	assert.NotContains(t, ce.Message, "log in")
	assert.Empty(t, ce.Hint)
}

// pluginStoredLoginOverNormanRefusal runs get-token for a stored plugin login
// with --user-id and no --cluster against a Rancher whose only token create is
// the Norman one and answers 403, and returns the error.
func pluginStoredLoginOverNormanRefusal(t *testing.T) *clierr.Error {
	t.Helper()
	x := storedLoginFixture(t, testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateStatus: http.StatusForbidden}, "user-e2e", clierr.ProvenanceExecLogin)
	_, err := x.getTokenNoCluster(t, x.entryName(), "user-e2e")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	return ce
}

// TestStoredLoginForbiddenForAClusterIsClusterAccess asserts a 403 on the
// token a stored login creates for a cluster is worded as cluster access, as
// the sign-in words it, and never as a missing role that creates tokens: the
// stored login created a token when it logged in, and the grant of the global
// role user would not change the answer.
func TestStoredLoginForbiddenForAClusterIsClusterAccess(t *testing.T) {
	x := newPluginFixture(t, true, testrancher.Options{ScopedCreateStatus: http.StatusForbidden})
	cred, err := AcquireLoginCredential(t.Context(), x.client(), LoginRequest{CAFlag: "--cacert", Server: x.url, AuthProvider: "localProvider", ResponseType: "json"}, 0, localPrompter())
	require.NoError(t, err)
	seedEntry(t, x.dir, x.store, x.entryName(), config.Server{URL: x.url, Provenance: clierr.ProvenanceExecLogin, TokenRef: cred.Ref, TokenUserID: "user-e2e"}, cred.Token, true)
	_, err = x.getToken(t, "c-test", "n", localPrompter())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.True(t, strings.HasSuffix(ce.Message, "; "+fmt.Sprintf(grantCluster, "'c-test'")), ce.Message)
	assert.NotContains(t, ce.Message, "global role")
	assert.False(t, clierr.HasRemedy(ce.Message))
	assert.Empty(t, ce.Hint)
}

// forbiddenEntry is one way a person meets a 403 on creating a token for
// Rancher itself, driven through the test Rancher: text runs the entry and
// returns the text it shows; functions are the functions of the package that
// handle the 403 for it.
type forbiddenEntry struct {
	name      string
	functions []string
	text      func(t *testing.T) (shown, want string)
}

// forbiddenEntries is every entry that can meet a 403 on creating a token for
// Rancher itself, with the permission text each must show.
func forbiddenEntries() []forbiddenEntry {
	acquire := func(server string) func(t *testing.T) (string, string) {
		return func(t *testing.T) (string, string) {
			t.Helper()
			_, d := forbiddenAcquire(t, testrancher.Options{LegacyLoginBodyUserID: true}, server)
			return d.Recovery, noTokenPermissionText("user-e2e", true)
		}
	}
	signIn := func(opts testrancher.Options, ext bool) func(t *testing.T) (string, string) {
		return func(t *testing.T) (string, string) {
			t.Helper()
			_, _, _, err := signInAgainst(t, opts, "")
			_, d := failureOf(t, err)
			return d.Recovery, noTokenPermissionText("", ext)
		}
	}
	return []forbiddenEntry{
		{name: "auth login", functions: []string{"FailAcquire"}, text: acquire("prod")},
		{name: "plugin login", functions: []string{"FailAcquire"}, text: acquire(config.ExecLoginPrefix + "r.example.com")},
		{name: "sign-in over ext", functions: []string{"unscopedCreateRefusal"}, text: signIn(testrancher.Options{DerivedCreateStatus: http.StatusForbidden}, true)},
		{name: "sign-in over Norman", functions: []string{"unscopedCreateRefusal", "signInTokenError"},
			text: signIn(testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateStatus: http.StatusForbidden}, false)},
		{name: "get-token with a stored plugin login over Norman, no cluster", functions: []string{"tokenCmd.storedCredToken", "tokenCmd.classifyRejection"}, text: func(t *testing.T) (string, string) {
			t.Helper()
			return pluginStoredLoginOverNormanRefusal(t).Message, noTokenPermissionText("user-e2e", false)
		}},
		{name: "get-token with a stored login, no cluster", functions: []string{"tokenCmd.storedCredToken", "tokenCmd.classifyRejection"}, text: func(t *testing.T) (string, string) {
			t.Helper()
			x := storedLoginFixture(t, testrancher.Options{DerivedCreateStatus: http.StatusForbidden}, "user-e2e", clierr.ProvenanceDerived)
			_, err := x.getTokenNoCluster(t, "prod", "user-e2e")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			return ce.Message, noTokenPermissionText("user-e2e", true)
		}},
	}
}

// TestEveryTokenCreateRefusalCarriesThePermissionText asserts each entry that
// can meet a 403 on creating a token for Rancher itself shows the text the one
// shared function builds.
func TestEveryTokenCreateRefusalCarriesThePermissionText(t *testing.T) {
	for _, e := range forbiddenEntries() {
		t.Run(e.name, func(t *testing.T) {
			shown, want := e.text(t)
			assert.Contains(t, shown, want)
		})
	}
}

// forbiddenAllowlist is each function of the package that names the forbidden
// status or errCredentialForbidden without handling a 403 on creating a token
// for Rancher itself, with why. A function new to the package that names
// either is handled by an entry of forbiddenEntries or listed here.
var forbiddenAllowlist = map[string]string{
	"createRefusal":              "cluster access, not token permission",
	"freshCredentialError":       "reached only after this login created its token",
	"probeDirect":                "readiness probe, not a token create",
	"revocationItem.judge":       "revoke answered 403, not a token create",
	"loginError":                 "password login refused, not a token create",
	"var errCredentialForbidden": "the declaration of the sentinel",
	"postTokenCreate":            "the wrap that produces the sentinel",
	"rancherMessage":             "text trim",
}

// TestEveryForbiddenReferenceIsCoveredByThePermissionText asserts every
// reference in the package's non-test source to http.StatusForbidden or
// errCredentialForbidden, comments excluded, sits in a function an entry of
// forbiddenEntries covers or in forbiddenAllowlist: a new place that
// handles a refused token create cannot skip the permission text.
func TestEveryForbiddenReferenceIsCoveredByThePermissionText(t *testing.T) {
	t.Parallel()
	covered := map[string]bool{}
	for _, e := range forbiddenEntries() {
		for _, fn := range e.functions {
			covered[fn] = true
		}
	}
	hits := forbiddenReferences(t, ".")
	require.NotEmpty(t, hits)
	for fn, files := range hits {
		if covered[fn] {
			continue
		}
		if reason, ok := forbiddenAllowlist[fn]; ok {
			assert.NotEmpty(t, reason)
			continue
		}
		t.Errorf("%s (%s) names http.StatusForbidden or errCredentialForbidden and is in no entry of forbiddenEntries and not in forbiddenAllowlist", fn, strings.Join(files, ", "))
	}
	for fn := range covered {
		assert.Contains(t, hits, fn, "forbiddenEntries names a function with no reference: %s", fn)
	}
	for fn := range forbiddenAllowlist {
		assert.Contains(t, hits, fn, "forbiddenAllowlist names a function with no reference: %s", fn)
	}
}

// forbiddenReferences returns, for each function of the non-test files of dir
// that names http.StatusForbidden or errCredentialForbidden, the files it does
// it in. A function is "Name" or "Receiver.Name"; a reference outside any
// function is named by what the declaration declares, for example
// "var errCredentialForbidden".
func forbiddenReferences(t *testing.T, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	hits := map[string][]string{}
	note := func(fn, file string) {
		if !slices.Contains(hits[fn], file) {
			hits[fn] = append(hits[fn], file)
		}
	}
	fset := gotoken.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		require.NoError(t, err)
		inspect := func(fn string, node ast.Node) {
			ast.Inspect(node, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if x, ok := n.X.(*ast.Ident); ok && x.Name == "http" && n.Sel.Name == "StatusForbidden" {
						note(fn, name)
					}
				case *ast.Ident:
					if n.Name == "errCredentialForbidden" {
						note(fn, name)
					}
				}
				return true
			})
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				fn := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					fn = receiverName(d.Recv.List[0].Type) + "." + fn
				}
				inspect(fn, d)
			case *ast.GenDecl:
				// A package-level declaration is named by the names each of its
				// specs declares, so a new handler assigned to a package
				// variable has a key of its own that no list holds.
				for _, spec := range d.Specs {
					var names []string
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, n := range vs.Names {
							names = append(names, d.Tok.String()+" "+n.Name)
						}
					}
					inspect(strings.Join(names, ", "), spec)
				}
			}
		}
	}
	return hits
}

// receiverName is the type name of a method receiver, without a pointer.
func receiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverName(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		return receiverName(e.X)
	}
	return ""
}
