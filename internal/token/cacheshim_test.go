package token

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheShimCachesWhatItMadeOnAMiss asserts the store check is made
// against the store the credential was actually read from. The legacy
// shim probes the cache file directly, so its lazy store resolves only at
// the credential read that follows the binding's load; a check that took
// the store before that read would find none and refuse every record.
func TestTheShimCachesWhatItMadeOnAMiss(t *testing.T) {
	m := newMadeFrom(t)
	for _, tc := range []struct{ name, decl, pin string }{
		{"a declared store", "file", ""},
		{"auto, pinned", "auto", "file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(config.EnvCredentialStore, "")
			cfg := &config.Config{CredentialStore: tc.decl, Servers: map[string]config.Server{"prod": {URL: m.URL}}}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())
			if tc.pin != "" {
				require.NoError(t, config.WritePin(dir, tc.pin))
			}
			require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "prod", URL: m.URL}, "alice"))

			shim := func() (string, string) {
				var notices bytes.Buffer
				p := ui.New(&notices, strings.NewReader(""), true, false)
				res, err := NewLegacyTokenCommand(dir, nil, anchorFor(dir)).(runtime.ReadCommand).Execute(t.Context(),
					tin{s: map[string]string{"server": m.URL, "user": "prod", "cluster": "local"}}, p)
				require.NoError(t, err)
				return res.Data.(*execCredential).Status.Token, notices.String()
			}
			tok, notices := shim()
			assert.Equal(t, testrancher.MadeFromToken("alice"), tok)
			assert.NotContains(t, notices, "not caching")
			before := m.Creates()
			tok, _ = shim()
			assert.Equal(t, testrancher.MadeFromToken("alice"), tok)
			assert.Equal(t, before, m.Creates(), "the second call is served from the record the first wrote")
		})
	}
}

// TestTheShimCachesItsInteractiveFallback asserts the same for the record
// the shim writes after an interactive sign-in: with an entry whose store
// held nothing, the store was read - and resolved - after the binding's
// load; with no entry for the server at all, no store was read, and a
// record bound to no entry is served only while there is none, so there
// is nothing for the check to compare.
func TestTheShimCachesItsInteractiveFallback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry bool
	}{{"an entry whose store holds nothing", true}, {"no entry for the server", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newTokenFacade(t, testrancher.Options{})
			dir := t.TempDir()
			t.Setenv(config.EnvCredentialStore, "")
			servers := map[string]config.Server{}
			if tc.entry {
				servers["prod"] = config.Server{URL: srv.URL}
			}
			cfg := &config.Config{CredentialStore: "file", Servers: servers}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())

			shim := func() (string, string) {
				var notices bytes.Buffer
				p := ui.New(&notices, newLineReader("e2e-user", "e2e-pass"), false, true)
				res, err := NewLegacyTokenCommand(dir, nil, anchorFor(dir)).(runtime.ReadCommand).Execute(t.Context(),
					tin{s: map[string]string{"server": srv.URL, "user": "prod", "cluster": "local", "auth-provider": "localProvider"}}, p)
				require.NoError(t, err)
				return res.Data.(*execCredential).Status.Token, notices.String()
			}
			tok, notices := shim()
			assert.NotContains(t, notices, "not caching")
			logins := f.LoginCount()
			again, _ := shim()
			assert.Equal(t, tok, again, "the second call is served from the record the first wrote")
			assert.Equal(t, logins, f.LoginCount(), "without signing in again")
		})
	}
}

// TestTheShimCachesItsFallbackWhenNoStoreWasRead asserts the shim's
// fallback record when the only entry for the server is a plugin-created
// one and token reuse is off: the lookup skips that entry and reads no
// store, so its lazy store never resolves, but the record is bound to
// that entry. No store was read, so no previous credential can be in the
// record, and it is written: one sign-in serves two calls.
func TestTheShimCachesItsFallbackWhenNoStoreWasRead(t *testing.T) {
	f, srv := newTokenFacade(t, testrancher.Options{})
	dir := t.TempDir()
	t.Setenv(config.EnvCredentialStore, "")
	cfg := &config.Config{CredentialStore: "file", Servers: map[string]config.Server{
		"exec-login": {URL: srv.URL, Provenance: clierr.ProvenanceExecLogin, CacheBindingID: "b1"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	shim := func() (string, string) {
		var notices bytes.Buffer
		p := ui.New(&notices, newLineReader("e2e-user", "e2e-pass"), false, true)
		res, err := NewLegacyTokenCommand(dir, nil, anchorFor(dir)).(runtime.ReadCommand).Execute(t.Context(),
			tin{s: map[string]string{"server": srv.URL, "user": "exec-login", "cluster": "local", "auth-provider": "localProvider"}}, p)
		require.NoError(t, err)
		return res.Data.(*execCredential).Status.Token, notices.String()
	}
	tok, notices := shim()
	assert.NotContains(t, notices, "not caching")
	again, _ := shim()
	assert.Equal(t, tok, again, "the second call is served from the record the first wrote")
	assert.Equal(t, 1, f.LoginCount(), "one sign-in over two calls")
}
