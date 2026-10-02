package config

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// loginRemedyBuilders lists the functions of this package that can write
// an auth login command, read from its source: every function whose body
// names LoginCommand or LoginHintFor, or holds a string literal with
// "auth login" in it.
func loginRemedyBuilders(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	var out []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, err)
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			writes := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.Ident:
					writes = writes || x.Name == "LoginCommand" || x.Name == "LoginHintFor"
				case *ast.BasicLit:
					writes = writes || (x.Kind == token.STRING && strings.Contains(x.Value, "auth login"))
				}
				return !writes
			})
			if writes {
				out = append(out, fn.Name.Name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// textOf is the message and hint of err.
func textOf(err error) []string {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok {
		if err == nil {
			return nil
		}
		return []string{err.Error()}
	}
	return []string{ce.Message, ce.Hint}
}

// TestNoLoginRemedyNamesAPluginLoginEntry asserts no remedy this package
// writes runs auth login on a name in the exec plugin's own login entry
// namespace, which auth login refuses: every function that can write an
// auth login command, found in the source, is run with such a name
// wherever it takes an entry name, and its text is searched. A function
// added without a row here fails the test.
func TestNoLoginRemedyNamesAPluginLoginEntry(t *testing.T) {
	const name = "exec-login-r.ex"
	dir := t.TempDir()
	rows := map[string]func(t *testing.T) []string{
		"LoginHintFor": func(*testing.T) []string { return []string{LoginHintFor(name)} },
		"ErrNotLoggedInFor": func(*testing.T) []string {
			return append(textOf(ErrNotLoggedInFor(map[string]string{name: "https://r.ex"})),
				textOf(ErrNotLoggedInFor(map[string]string{name: "https://r.ex", "prod": "https://r.ex"}))...)
		},
		"credentialReplacedError": func(*testing.T) []string { return textOf(credentialReplacedError(name)) },
		"loginIncompleteError":    func(*testing.T) []string { return textOf(loginIncompleteError(name)) },
		"identityError":           func(*testing.T) []string { return textOf(identityError(dir, name, "unreadable")) },
		"identityListError":       func(*testing.T) []string { return textOf(identityListError(dir, name, "is unreadable")) },
		"identityDirError":        func(*testing.T) []string { return textOf(identityDirError(dir, "unreadable")) },
		"TextLines": func(*testing.T) []string {
			// Prose naming the command, with no entry name in it.
			var out []string
			for _, st := range []string{ChangeRecordActive, ChangeRecordAbandoned} {
				out = append(out, (&ChangeRecordStatus{State: st, ID: name}).TextLines()...)
			}
			return out
		},
		"persistUnderLocks": func(t *testing.T) []string {
			t.Helper()
			// The plugin-created login's own entry, failing at the write
			// that records an accepted certificate: an error whose hint
			// is the login of the entry being written.
			keyring.MockInit()
			pdir, _ := persistFixture(t, nil)
			in := pluginInput(pdir, name, NewFileStore(pdir))
			in.PendingTrust = &PendingTrust{CA: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n", Fingerprint: "f1"}
			t.Cleanup(SetPersistWriteHookForTest(func(step string) error {
				if step == "trust" {
					return errors.New("config directory is full")
				}
				return nil
			}))
			_, err := PersistLoginCredential(t.Context(), pdir, in)
			require.ErrorContains(t, err, "storing the accepted certificate for "+name, "the row reaches the hint it guards")
			return textOf(err)
		},
	}
	builders := loginRemedyBuilders(t)
	t.Logf("functions that can write an auth login command: %s", strings.Join(builders, ", "))
	assert.Equal(t, builders, slices.Sorted(maps.Keys(rows)), "every such function has a row, and every row is one")
	loginOnPlugin := regexp.MustCompile(`auth login '?` + regexp.QuoteMeta(ExecLoginPrefix))
	for builder, run := range rows {
		for _, text := range run(t) {
			assert.NotRegexp(t, loginOnPlugin, clierr.ExpandRemedies(text, ""), builder)
		}
	}
}

// TestNotLoggedInWithOnlyAPluginLoginEntryNamesALoginOfANewEntry asserts
// the refusal when the directory's one server entry is the exec plugin's
// own login entry and none is current: auth login refuses its name, so the
// message and the hint name a login under the name derived from the
// server's host and port, given in the command since auth login derives
// none from an IPv6 address. The hint for an IPv6 server is run through
// auth login by TestNotLoggedInHintForALonePluginLoginEntryRuns.
func TestNotLoggedInWithOnlyAPluginLoginEntryNamesALoginOfANewEntry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		entry, url, login string
	}{
		{"exec-login-r.ex", "https://r.ex", "rancher auth login 'r.ex' --url 'https://r.ex'"},
		{"exec-login-r.ex-8443", "https://r.ex:8443", "rancher auth login 'r.ex-8443' --url 'https://r.ex:8443'"},
		{"exec-login-2001-db8--1-8443", "https://[2001:db8::1]:8443", "rancher auth login '2001-db8--1-8443' --url 'https://[2001:db8::1]:8443'"},
		{"exec-login---1-8443", "https://[::1]:8443", "rancher auth login '1-8443' --url 'https://[::1]:8443'"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			ce := ErrNotLoggedInFor(map[string]string{tc.entry: tc.url})
			assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
			assert.Equal(t, "not logged in - no current server; the only server entry, '"+tc.entry+"', is the exec plugin's own login entry, which auth login refuses; "+
				"log in under a server entry of your own with: "+tc.login, clierr.ExpandRemedies(ce.Message, ""))
			assert.Equal(t, tc.login, clierr.ExpandRemedies(ce.Hint, ""))
		})
	}
	assert.Empty(t, LoginHintFor("exec-login-r.ex"), "auth login refuses the name, so it is no hint")
	assert.Equal(t, "rancher auth login 'prod'", clierr.ExpandRemedies(LoginHintFor("prod"), ""))
	assert.Equal(t, "rancher auth login 'prod'", clierr.ExpandRemedies(ErrNotLoggedInFor(map[string]string{"prod": "https://r.ex"}).Hint, ""))
}
