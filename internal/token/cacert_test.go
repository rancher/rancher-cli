package token

import (
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

func entryFrom(name string) config.CASource {
	return config.CASource{Kind: config.CASourceEntry, Entry: name}
}

// TestResolveConnCAOrderWithTheVariable asserts the order of the CA a call
// connects with: --cacert, then RANCHER_CLI_CACERT, then the saved CA of the
// identified or matching entry, then the current entry's, and what it
// returns as the CA's source. The variable wins over an identified entry's
// CA and over a matching entry's, with or without --server; the shim reads
// it unless --skip-verify; RANCHER_CLI_URL naming another server skips it.
// The entry identification is the same with and without the variable.
func TestResolveConnCAOrderWithTheVariable(t *testing.T) {
	caProd := selfSignedCAPEM(t)
	caEU := selfSignedCAPEM(t)
	caFlag := selfSignedCAPEM(t)
	caVar := selfSignedCAPEM(t)
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{
		"prod":       {URL: "https://r.ex", CA: caProd},
		"prod-eu":    {URL: "https://r.ex", CA: caEU},
		"prod-plain": {URL: "https://r.ex"},
		"prod-elsew": {URL: "https://elsewhere.ex", CA: caEU},
	}}
	dir := t.TempDir()
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	variable := caVar
	cases := []struct {
		name, server, wantName, cacert, rancherURL string
		shim, skipVerify, noServer                 bool
		withVariable, withoutVariable              string
		fromWith, fromWithout                      config.CASource
		wantCode                                   string
	}{
		{name: "a server named, a matching entry with a CA", withVariable: caVar, fromWith: config.CASource{Kind: config.CASourceVariable},
			withoutVariable: caProd, fromWithout: entryFrom("prod")},
		{name: "a server named that no entry matches", server: "https://nowhere.ex", withVariable: caVar, fromWith: config.CASource{Kind: config.CASourceVariable}},
		{name: "no server named, the current entry", noServer: true, withVariable: caVar, fromWith: config.CASource{Kind: config.CASourceVariable},
			withoutVariable: caProd, fromWithout: entryFrom("prod")},
		{name: "no server named, a --name that identifies an entry", noServer: true, wantName: "prod-eu-c-1", withVariable: caVar,
			fromWith: config.CASource{Kind: config.CASourceVariable}, withoutVariable: caEU, fromWithout: entryFrom("prod-eu")},
		{name: "a server named, a --name that identifies an entry", wantName: "prod-eu-c-1", withVariable: caVar,
			fromWith: config.CASource{Kind: config.CASourceVariable}, withoutVariable: caEU, fromWithout: entryFrom("prod-eu")},
		{name: "an identified entry with no CA", wantName: "prod-plain-c-1", withVariable: caVar, fromWith: config.CASource{Kind: config.CASourceVariable}},
		{name: "--cacert beats the variable", cacert: caFlag, withVariable: caFlag, fromWith: config.CASource{Kind: config.CASourceFlag},
			withoutVariable: caFlag, fromWithout: config.CASource{Kind: config.CASourceFlag}},
		{name: "--cacert with no server named", noServer: true, cacert: caFlag, withVariable: caFlag, fromWith: config.CASource{Kind: config.CASourceFlag},
			withoutVariable: caFlag, fromWithout: config.CASource{Kind: config.CASourceFlag}},
		{name: "the shim reads the variable", shim: true, wantName: "prod-eu", withVariable: caVar, fromWith: config.CASource{Kind: config.CASourceVariable},
			withoutVariable: caProd, fromWithout: entryFrom("prod")},
		{name: "the shim with --skip-verify does not read it", shim: true, skipVerify: true, wantName: "prod-eu", withVariable: caProd, fromWith: entryFrom("prod"),
			withoutVariable: caProd, fromWithout: entryFrom("prod")},
		{name: "RANCHER_CLI_URL names this server", rancherURL: "https://r.ex/", withVariable: caVar, fromWith: config.CASource{Kind: config.CASourceVariable},
			withoutVariable: caProd, fromWithout: entryFrom("prod")},
		{name: "RANCHER_CLI_URL names another server, a server named", rancherURL: "https://other.example", withVariable: caProd, fromWith: entryFrom("prod"),
			withoutVariable: caProd, fromWithout: entryFrom("prod")},
		{name: "RANCHER_CLI_URL names another server, no server named", noServer: true, rancherURL: "https://other.example",
			withVariable: caVar, fromWith: config.CASource{Kind: config.CASourceVariable}},
		{name: "an identified entry at another URL stops the call with or without it", wantName: "prod-elsew-c-1", wantCode: clierr.CodeConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvURL, tc.rancherURL)
			server := tc.server
			switch {
			case tc.noServer:
				server = ""
			case server == "":
				server = "https://r.ex"
			}
			call := func(t *testing.T, v string) (source string, from config.CASource, choice entryChoice, err error) {
				t.Helper()
				t.Setenv(config.EnvCACert, v)
				cmd := tokenCmd{dir: dir, store: config.NewFileStore(dir)}
				_, _, _, source, from, choice, err = cmd.resolveConnWithCA(server, "c-1", tc.cacert, tc.wantName, "", tc.shim, tc.skipVerify)
				return source, from, choice, err
			}
			withSource, withFrom, withChoice, err := call(t, variable)
			withoutSource, withoutFrom, withoutChoice, errWithout := call(t, "")
			if tc.wantCode != "" {
				for _, e := range []error{err, errWithout} {
					ce, ok := clierrOf(e)
					require.True(t, ok, "%v", e)
					assert.Equal(t, tc.wantCode, ce.Code)
				}
				return
			}
			require.NoError(t, err)
			require.NoError(t, errWithout)
			assert.Equal(t, tc.withVariable, withSource)
			assert.Equal(t, tc.fromWith, withFrom)
			assert.Equal(t, tc.withoutVariable, withoutSource)
			assert.Equal(t, tc.fromWithout, withoutFrom)
			assert.Equal(t, withoutChoice, withChoice, "the entry identification is the same whatever supplies the CA")
		})
	}
}

func clierrOf(err error) (*clierr.Error, bool) {
	return errors.AsType[*clierr.Error](err)
}

// TestResolveConnCAReadsTheVariableAsAPathOrAPEM asserts the variable's value
// is resolved like --cacert's: a file path is read, a PEM is used as is.
func TestResolveConnCAReadsTheVariableAsAPathOrAPEM(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex"}}, CurrentServer: "prod"}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	ca := selfSignedCAPEM(t)
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, []byte(ca), 0o600))
	for name, v := range map[string]string{"a path": path, "a PEM": ca} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(config.EnvCACert, v)
			_, pemOut, _, source, _, _, err := tokenCmd{dir: dir, store: config.NewFileStore(dir)}.resolveConnWithCA("https://r.ex", "c-1", "", "", "", false, false)
			require.NoError(t, err)
			assert.Equal(t, v, source)
			assert.Equal(t, ca, pemOut)
		})
	}
}

// TestResolveConnCANamesTheSourceOfAnUnreadableCA asserts the read error
// names where the CA came from: --cacert, --cacerts for the legacy shim,
// RANCHER_CLI_CACERT, or the server entry that saves it.
func TestResolveConnCANamesTheSourceOfAnUnreadableCA(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.pem")
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{
		"prod": {URL: "https://r.ex", CA: missing},
		"bare": {URL: "https://bare.ex"},
	}, CurrentServer: "prod"}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	for _, tc := range []struct {
		name, server, cacert, variable string
		shim                           bool
		want                           string
	}{
		{"the get-token flag", "https://r.ex", missing, "", false, "reading --cacert: "},
		{"the shim's flag", "https://r.ex", missing, "", true, "reading --cacerts: "},
		{"the variable", "https://bare.ex", "", missing, false, "reading RANCHER_CLI_CACERT: "},
		{"the variable, for the shim", "https://bare.ex", "", missing, true, "reading RANCHER_CLI_CACERT: "},
		{"a server entry's saved CA", "https://r.ex", "", "", false, "reading the CA of server entry 'prod': "},
		{"a server entry's saved CA, for the shim", "https://r.ex", "", "", true, "reading the CA of server entry 'prod': "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvCACert, tc.variable)
			_, _, _, _, _, _, err := tokenCmd{dir: dir, store: config.NewFileStore(dir)}.resolveConnWithCA(tc.server, "c-1", tc.cacert, "", "", tc.shim, false)
			ce, ok := clierrOf(err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, "missing.pem")
		})
	}
}

// TestTheChosenCAIsTheOneTheConnectionTrusts asserts, with the production
// client against a TLS server, that the CA resolveConnWithCA returns is the
// one the request is verified against: the variable reaches a server whose
// entry saves a CA that does not fit it, and a variable that does not fit
// fails where the saved CA would have worked.
func TestTheChosenCAIsTheOneTheConnectionTrusts(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(srv.Close)
	right := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	wrong := selfSignedCAPEM(t)

	reach := func(t *testing.T, savedCA, variable string) error {
		t.Helper()
		dir := t.TempDir()
		cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL, CA: savedCA}}, CurrentServer: "prod"}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		t.Setenv(config.EnvCACert, variable)
		_, caPEM, _, _, _, _, err := tokenCmd{dir: dir, store: config.NewFileStore(dir)}.resolveConnWithCA(srv.URL, "c-1", "", "", "", false, false)
		require.NoError(t, err)
		client, err := httpx.New(httpx.Options{CACert: caPEM, Timeout: httpx.DefaultTimeout})
		require.NoError(t, err)
		resp, err := client.Get(srv.URL)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	require.NoError(t, reach(t, wrong, right), "the variable wins over a saved CA that does not fit")
	require.NoError(t, reach(t, right, ""), "no variable: the saved CA")
	require.ErrorContains(t, reach(t, right, wrong), "certificate", "the variable wins over a saved CA that would have fit")
	require.ErrorContains(t, reach(t, wrong, ""), "certificate")
}

// TestAResolvedPathHitWithTheVariableSetIsServedFromTheCache asserts the
// plugin's cache hit on the resolved path costs what it did, one config load
// and the confirming stamp read, with the variable set as a path or a PEM,
// builds no client, and fails loudly on an unreadable variable path before
// it reads the cache; the flag fast path never reads the variable.
func TestAResolvedPathHitWithTheVariableSetIsServedFromTheCache(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://r.ex"), "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))
	t.Setenv(execInfoEnv, execInfoFor(t, "https://r.ex/k8s/clusters/c-1", nil))

	ca := selfSignedCAPEM(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte(ca), 0o600))
	run := func(t *testing.T, variable string, anchor *config.CacheAnchor) (map[string]int, runtime.Result, error) {
		t.Helper()
		t.Setenv(config.EnvCACert, variable)
		ops := map[string]int{}
		restore := config.SetIdentityObserverForTest(func(op string) { ops[op]++ })
		defer restore()
		cmd := tokenCmd{dir: dir, store: store, anchor: anchor,
			newClient: func(httpx.Options) (*http.Client, error) {
				require.Fail(t, "a cache hit builds no client")
				return nil, nil
			}}
		res, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": "https://r.ex", "name": "prod", "cluster": "c-1"}}, ui.Discard())
		return ops, res, err
	}
	for name, v := range map[string]string{"a path": caPath, "a PEM": ca} {
		t.Run(name, func(t *testing.T) {
			ops, res, err := run(t, v, anchorFor(dir))
			require.NoError(t, err)
			assert.Equal(t, "cached-tok", res.Data.(*execCredential).Status.Token)
			assert.Equal(t, map[string]int{"configload": 1, "stampread": 1}, ops)
		})
	}
	t.Run("an unreadable path fails the hit", func(t *testing.T) {
		_, _, err := run(t, filepath.Join(t.TempDir(), "gone.pem"), anchorFor(dir))
		require.ErrorContains(t, err, "reading RANCHER_CLI_CACERT: ")
	})
	t.Run("the flag fast path never reads the variable", func(t *testing.T) {
		t.Setenv(execInfoEnv, "")
		ops, res, err := run(t, filepath.Join(t.TempDir(), "gone.pem"), anchorFor(dir))
		require.NoError(t, err, "the unreadable variable is not read on the fast path")
		assert.Equal(t, "cached-tok", res.Data.(*execCredential).Status.Token)
		assert.Equal(t, 1, ops["stampread"])
		assert.Zero(t, ops["configload"])
	})
}

// TestThePluginSignInSavesTheCAItConnectedWith asserts the plugin's own
// sign-in saves in its new exec-login entry the CA the call connected with,
// the variable's included for a named server, and none for the shim's
// --skip-verify, which reads no variable.
func TestThePluginSignInSavesTheCAItConnectedWith(t *testing.T) {
	ca := selfSignedCAPEM(t)
	t.Run("the variable", func(t *testing.T) {
		x := newPluginFixture(t, true, testrancher.Options{})
		t.Setenv(config.EnvCACert, ca)
		_, err := x.getToken(t, "c-test", "n", localPrompter())
		require.NoError(t, err)
		s, ok := x.entry(t)
		require.True(t, ok)
		assert.Equal(t, ca, s.CA, "the variable's PEM is what the sign-in connected with")
		assert.False(t, s.CAFromEnvironment)
	})
	t.Run("no variable", func(t *testing.T) {
		x := newPluginFixture(t, true, testrancher.Options{})
		_, err := x.getToken(t, "c-test", "n", localPrompter())
		require.NoError(t, err)
		s, ok := x.entry(t)
		require.True(t, ok)
		assert.Empty(t, s.CA)
	})
	t.Run("the shim with --skip-verify", func(t *testing.T) {
		x := newPluginFixture(t, true, testrancher.Options{})
		t.Setenv(config.EnvCACert, ca)
		_, err := x.command(x.store).runWithParams(t.Context(), runParams{
			Server: x.url, Name: config.LegacyCachePrefix + "n", Cluster: "c-test", AuthProvider: "localProvider",
			SkipVerify: true, legacyShim: true,
		}, localPrompter())
		require.NoError(t, err)
		s, ok := x.entry(t)
		require.True(t, ok)
		assert.Empty(t, s.CA, "no variable CA is saved when verification is skipped")
	})
	t.Run("the shim without it", func(t *testing.T) {
		x := newPluginFixture(t, true, testrancher.Options{})
		t.Setenv(config.EnvCACert, ca)
		_, err := x.command(x.store).runWithParams(t.Context(), runParams{
			Server: x.url, Name: config.LegacyCachePrefix + "n", Cluster: "c-test", AuthProvider: "localProvider", legacyShim: true,
		}, localPrompter())
		require.NoError(t, err)
		s, ok := x.entry(t)
		require.True(t, ok)
		assert.Equal(t, ca, s.CA, "the shim names its server, so the variable applies")
	})
}

// TestNoTerminalLoginNamesWhereAnInlineCAIsCarried asserts the login a
// no-terminal error names for a CA that is inline: from the variable, the
// clause says RANCHER_CLI_CACERT carries it; from a server entry, that
// entry; from a flag, the exec entry. A path is named in the command, and an
// entry already registered at the server names no --cacert.
func TestNoTerminalLoginNamesWhereAnInlineCAIsCarried(t *testing.T) {
	const url = "https://r.ex"
	inline := "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n"
	const lead = ", adding --cacert with the path of a file that holds the server's CA certificate, which "
	for _, tc := range []struct {
		name string
		ca   string
		from config.CASource
		want string
	}{
		{"the variable", inline, config.CASource{Kind: config.CASourceVariable}, lead + "RANCHER_CLI_CACERT carries inline"},
		{"a server entry", inline, entryFrom("other"), lead + "server entry 'other' carries inline in config.yaml"},
		{"the flag", inline, config.CASource{Kind: config.CASourceFlag}, lead + "this exec entry carries inline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NoTerminalLogin(nil, "", "c-1", url, tc.ca, tc.from, false)
			assert.True(t, l.InlineCA)
			assert.Contains(t, l.Text(), tc.want)
			assert.Empty(t, l.Hint())
		})
	}
	t.Run("a path from the variable", func(t *testing.T) {
		l := NoTerminalLogin(nil, "prod-c-1", "c-1", url, "/etc/ca.pem", config.CASource{Kind: config.CASourceVariable}, false)
		assert.False(t, l.InlineCA)
		assert.Contains(t, l.Command, " --cacert '/etc/ca.pem'")
	})
	t.Run("an entry registered here names no CA", func(t *testing.T) {
		cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: url}}}
		l := NoTerminalLogin(cfg, "prod-c-1", "c-1", url, inline, config.CASource{Kind: config.CASourceVariable}, false)
		assert.False(t, l.InlineCA)
		assert.NotContains(t, l.Command, "--cacert")
		assert.Equal(t, l.Command, l.Hint())
	})
}

// TestNoTerminalErrorNamesTheVariableThatCarriesTheCA asserts a get-token
// call without a terminal whose CA came from RANCHER_CLI_CACERT as an inline
// PEM names the variable as the holder, and as a path puts the path in the
// login the error hints.
func TestNoTerminalErrorNamesTheVariableThatCarriesTheCA(t *testing.T) {
	srv := localProviderServer(t)
	ca := selfSignedCAPEM(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte(ca), 0o600))
	call := func(t *testing.T, variable string) error {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "rancher")
		cfg := &config.Config{CredentialStore: "file"}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		t.Setenv(config.EnvCACert, variable)
		cmd := NewTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": srv.URL, "cluster": "c-1", "name": "prod-c-1"}}, noTerminal())
		require.Error(t, err)
		return err
	}
	err := call(t, ca)
	require.ErrorContains(t, err, "which RANCHER_CLI_CACERT carries inline")
	assert.NotContains(t, err.Error(), "this exec entry carries inline")

	err = call(t, caPath)
	require.ErrorContains(t, err, " --cacert "+clierr.ShellQuote(caPath))
	ce, ok := clierrOf(err)
	require.True(t, ok)
	assert.Contains(t, ce.Hint, " --cacert ", "the login runs as written")
}
