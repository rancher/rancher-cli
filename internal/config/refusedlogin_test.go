package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// provenanceValues reads the values of the Provenance constants that package
// clierr declares, from every non-test file of its source.
func provenanceValues(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("../clierr/*.go")
	require.NoError(t, err)
	var out []string
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		for _, d := range file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Provenance" {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					require.True(t, ok, "a Provenance constant is a string literal")
					s, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					out = append(out, s)
				}
			}
		}
	}
	require.NotEmpty(t, out)
	slices.Sort(out)
	return out
}

// TestRefusedLoginHint asserts the command a refused stored token names for
// every provenance value an entry holds, with and without a recorded auth
// config. A Provenance constant with no row fails the test.
func TestRefusedLoginHint(t *testing.T) {
	t.Parallel()
	const stdin = "rancher auth login 'prod' --token-stdin"
	const provider = "rancher auth login 'prod' --auth-provider 'corp-ldap'"
	rows := []struct {
		name       string
		provenance clierr.Provenance
		authConfig string
		want       string
	}{
		{"a pasted token", clierr.ProvenanceUserSupplied, "", stdin},
		{"a pasted token with a recorded auth config", clierr.ProvenanceUserSupplied, "corp-ldap", stdin},
		{"an entry with no provenance", "", "", stdin},
		{"an entry with no provenance and a recorded auth config", "", "corp-ldap", stdin},
		{"a derived token from an auth config", clierr.ProvenanceDerived, "corp-ldap", provider},
		{"a derived token with no recorded auth config", clierr.ProvenanceDerived, "", stdin},
		{"a legacy session from an auth config", clierr.ProvenanceLegacySession, "corp-ldap", provider},
		{"a legacy session with no recorded auth config", clierr.ProvenanceLegacySession, "", stdin},
		{"a provenance this version does not know", "future", "corp-ldap", stdin},
		{"the exec plugin's own login", clierr.ProvenanceExecLogin, "corp-ldap", ""},
		{"the exec plugin's own login with no auth config", clierr.ProvenanceExecLogin, "", ""},
	}
	listed := map[string]bool{}
	for _, r := range rows {
		listed[string(r.provenance)] = true
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			cmd, ok := RefusedLoginHint("prod", Server{Provenance: r.provenance, TokenAuthConfig: r.authConfig})
			if r.want == "" {
				assert.False(t, ok)
				return
			}
			require.True(t, ok)
			assert.True(t, cmd.OK())
			assert.Equal(t, r.want, clierr.ExpandRemedies(cmd.String(), ""))
		})
	}
	for _, v := range provenanceValues(t) {
		assert.True(t, listed[v], "the rows above list the provenance %q", v)
	}
	assert.True(t, listed[""], "the rows above list an entry with no provenance")
}

// TestRefusedLoginHintForNamesThatCannotBePrinted asserts the command is left
// out, with no hint text, when the entry name or the auth config name cannot
// be printed, and that an exec plugin login entry name gets none.
func TestRefusedLoginHintForNamesThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	t.Run("an entry name", func(t *testing.T) {
		t.Parallel()
		cmd, ok := RefusedLoginHint("bad\x00name", Server{Provenance: clierr.ProvenanceUserSupplied})
		require.True(t, ok)
		assert.False(t, cmd.OK())
		assert.Empty(t, cmd.Field())
	})
	t.Run("an auth config name", func(t *testing.T) {
		t.Parallel()
		cmd, ok := RefusedLoginHint("prod", Server{Provenance: clierr.ProvenanceDerived, TokenAuthConfig: "bad\x00name"})
		require.True(t, ok)
		assert.False(t, cmd.OK())
		assert.Empty(t, cmd.Field())
	})
	t.Run("an exec plugin login entry name", func(t *testing.T) {
		t.Parallel()
		for _, p := range []clierr.Provenance{"", clierr.ProvenanceUserSupplied, clierr.ProvenanceDerived} {
			_, ok := RefusedLoginHint("exec-login-r.ex", Server{Provenance: p})
			assert.False(t, ok, "provenance %q", p)
		}
	})
}

// TestEffectiveLoginRemedy asserts Effective carries the login command of the
// stored entry its token came from, and none when the token came from
// RANCHER_CLI_TOKEN, when RANCHER_CLI_URL names another server, or when the
// entry has no stored token.
func TestEffectiveLoginRemedy(t *testing.T) {
	cfg := &Config{
		CurrentServer: "prod",
		Servers: map[string]Server{"prod": {
			URL: "https://rancher.example.com", Provenance: clierr.ProvenanceDerived, TokenAuthConfig: "corp-ldap",
		}},
	}
	const want = "rancher auth login 'prod' --auth-provider 'corp-ldap'"
	store := &recordingStore{name: "prod", token: "stored-id:stored-secret"}

	t.Run("a stored token", func(t *testing.T) {
		eff, err := cfg.Effective(store)
		require.NoError(t, err)
		require.Equal(t, "stored-id:stored-secret", eff.Token)
		assert.Equal(t, want, clierr.ExpandRemedies(eff.LoginRemedy.String(), ""))
	})
	t.Run("RANCHER_CLI_TOKEN over a stored token", func(t *testing.T) {
		t.Setenv(EnvToken, "env-id:env-secret")
		eff, err := cfg.Effective(store)
		require.NoError(t, err)
		require.Equal(t, "env-id:env-secret", eff.Token)
		assert.Empty(t, eff.LoginRemedy.String(), "the zero Command")
	})
	t.Run("RANCHER_CLI_URL naming another server", func(t *testing.T) {
		t.Setenv(EnvURL, "https://other.example.com")
		eff, err := cfg.Effective(store)
		require.NoError(t, err)
		require.Empty(t, eff.Token)
		assert.Empty(t, eff.LoginRemedy.String(), "the zero Command")
	})
	t.Run("no stored token", func(t *testing.T) {
		eff, err := cfg.Effective(stubStore{tokenErr: ErrNoToken})
		require.NoError(t, err)
		require.Empty(t, eff.Token)
		assert.Empty(t, eff.LoginRemedy.String(), "the zero Command")
	})
	t.Run("the exec plugin's own login entry", func(t *testing.T) {
		plugin := &Config{
			CurrentServer: "exec-login-r.ex",
			Servers:       map[string]Server{"exec-login-r.ex": {URL: "https://rancher.example.com", Provenance: clierr.ProvenanceExecLogin}},
		}
		eff, err := plugin.Effective(&recordingStore{name: "exec-login-r.ex", token: "stored-id:stored-secret"})
		require.NoError(t, err)
		require.NotEmpty(t, eff.Token)
		assert.Empty(t, eff.LoginRemedy.String(), "the zero Command")
	})
}
