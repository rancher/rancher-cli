package token

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

const (
	getTokenKubeconfigNote = " (a kubeconfig the CLI wrote also carries the CA in certificate-authority-data: " +
		"save the CA on the server entry and write that kubeconfig again)"
)

// TestTLSRemedyForWordsEachGetTokenRow asserts the wording of a certificate
// failure of auth get-token and the legacy rancher token, row by row: the
// flag spelling, the kubeconfig sentence only for get-token, and the
// variable sentence. Mutations: change any string, or swap --cacert and
// --cacerts.
func TestTLSRemedyForWordsEachGetTokenRow(t *testing.T) {
	variable := config.CASource{Kind: config.CASourceVariable}
	for _, tc := range []struct {
		name string
		from config.CASource
		shim bool
		want tlstrust.Remedy
	}{
		{"get-token with no CA", config.CASource{}, false,
			tlstrust.Remedy{CA: " and pass it with --cacert" + getTokenKubeconfigNote, URL: "--server", Fix: "--cacert"}},
		{"get-token with --cacert", config.CASource{Kind: config.CASourceFlag}, false,
			tlstrust.Remedy{CA: " and pass it with --cacert" + getTokenKubeconfigNote, URL: "--server", Fix: "--cacert"}},
		{"get-token with an entry's CA", entryFrom("prod"), false,
			tlstrust.Remedy{CA: " and pass it with --cacert" + getTokenKubeconfigNote, URL: "--server", Fix: "--cacert"}},
		{"get-token with the variable", variable, false,
			tlstrust.Remedy{CA: "; the CA came from RANCHER_CLI_CACERT - pass --cacert <file>, or unset RANCHER_CLI_CACERT, or set it to the server's CA", URL: "--server", Fix: "--cacert"}},
		{"the shim with no CA", config.CASource{}, true,
			tlstrust.Remedy{CA: " and pass it with --cacerts", URL: "--server", Fix: "--cacerts"}},
		{"the shim with --cacerts", config.CASource{Kind: config.CASourceFlag}, true,
			tlstrust.Remedy{CA: " and pass it with --cacerts", URL: "--server", Fix: "--cacerts"}},
		{"the shim with the variable", variable, true,
			tlstrust.Remedy{CA: "; the CA came from RANCHER_CLI_CACERT - pass --cacerts <file>, or unset RANCHER_CLI_CACERT, or set it to the server's CA", URL: "--server", Fix: "--cacerts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tlsRemedyFor(tc.from, tc.shim))
		})
	}
}

// untrustedTokenServer is a TLS server whose built-in certificate no CA
// the test saves signs, answering every request with an empty object.
func untrustedTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func requireTrustErrorWith(t *testing.T, err error, remedy string) *clierr.Error {
	t.Helper()
	ce := tlstrust.TypedError(err)
	require.NotNil(t, ce, "got %v", err)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Equal(t, 10, ce.ExitCode())
	assert.False(t, ce.Retryable)
	d, ok := ce.Details.(tlstrust.Details)
	require.True(t, ok)
	assert.Equal(t, tlstrust.UnknownAuthorityMessage(d.IsLeafCertificate(), remedy), clierr.ExpandRemedies(ce.Message, ""))
	return ce
}

// TestGetTokenClassifiesACertificateFailure asserts auth get-token and the
// legacy rancher token, on a cache miss that must reach the server, fail
// with TLS_TRUST (exit 10, not retryable) worded for the command and the
// source of the CA, whatever that source is. Mutations: leave the client
// of clientFor unwrapped; restore the GENERIC flattening in
// postTokenCreate; fill the wrong row.
func TestGetTokenClassifiesACertificateFailure(t *testing.T) {
	keyring.MockInit()
	wrong := selfSignedCAPEM(t)
	variableRemedy := "; the CA came from RANCHER_CLI_CACERT - pass --cacert <file>, or unset RANCHER_CLI_CACERT, or set it to the server's CA"
	shimVariableRemedy := "; the CA came from RANCHER_CLI_CACERT - pass --cacerts <file>, or unset RANCHER_CLI_CACERT, or set it to the server's CA"
	for _, tc := range []struct {
		name     string
		shim     bool
		cacert   string
		savedCA  bool
		variable bool
		remedy   string
	}{
		{name: "get-token with no CA", remedy: " and pass it with --cacert" + getTokenKubeconfigNote},
		{name: "get-token with --cacert", cacert: wrong, remedy: " and pass it with --cacert" + getTokenKubeconfigNote},
		{name: "get-token with the entry's CA", savedCA: true, remedy: " and pass it with --cacert" + getTokenKubeconfigNote},
		{name: "get-token with the variable", variable: true, remedy: variableRemedy},
		{name: "the shim with no CA", shim: true, remedy: " and pass it with --cacerts"},
		{name: "the shim with --cacerts", shim: true, cacert: wrong, remedy: " and pass it with --cacerts"},
		{name: "the shim with the variable", shim: true, variable: true, remedy: shimVariableRemedy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(execInfoEnv, "")
			t.Setenv(config.EnvURL, "")
			t.Setenv(config.EnvCACert, "")
			t.Setenv(config.EnvCredentialStore, "")
			if tc.variable {
				t.Setenv(config.EnvCACert, wrong)
			}
			srv := untrustedTokenServer(t)
			dir := t.TempDir()
			store := config.NewFileStore(dir)
			entry := config.Server{URL: srv.URL}
			if tc.savedCA {
				entry.CA = wrong
			}
			cfg := &config.Config{CurrentServer: "prod", CredentialStore: "file", Servers: map[string]config.Server{"prod": entry}}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())
			require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: srv.URL}, "token-abc:secret"))

			var err error
			if tc.shim {
				flags := map[string]string{"server": srv.URL, "user": "prod", "cluster": "c-1"}
				if tc.cacert != "" {
					flags["cacerts"] = tc.cacert
				}
				_, err = NewLegacyTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand).
					Execute(t.Context(), tin{s: flags}, ui.Discard())
			} else {
				flags := map[string]string{"server": srv.URL, "cluster": "c-1", "name": "prod-c-1"}
				if tc.cacert != "" {
					flags["cacert"] = tc.cacert
				}
				_, err = NewTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand).
					Execute(t.Context(), tin{s: flags}, ui.Discard())
			}
			_ = requireTrustErrorWith(t, err, tc.remedy)
		})
	}
}

func wrappedTokenClient(t *testing.T, srv *httptest.Server, r tlstrust.Remedy) *http.Client {
	t.Helper()
	client, err := httpx.New(httpx.Options{Timeout: 5 * time.Second, WrapTransport: tlstrust.WrapTransportFor(srv.URL, r)})
	require.NoError(t, err)
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func plainTokenClient(t *testing.T) *http.Client {
	t.Helper()
	client, err := httpx.New(httpx.Options{Timeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(client.CloseIdleConnections)
	return client
}

var tokenSlotRemedy = tlstrust.Remedy{CA: "; CA-SLOT", URL: "URL-SLOT", Fix: "FIX-SLOT"}

func requireTypedTrust(t *testing.T, err error) *clierr.Error {
	t.Helper()
	ce := tlstrust.TypedError(err)
	require.NotNil(t, ce, "got %v", err)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Equal(t, 10, ce.ExitCode())
	assert.False(t, ce.Retryable)
	assert.Contains(t, ce.Message, "; CA-SLOT;", "the message carries the client's remedy")
	return ce
}

// TestFlatteningSitesReturnTheClassifiedError asserts each place that used
// to turn a transport error into GENERIC text returns the TLS_TRUST error
// of the wrapped client unchanged: the token create, the plugin's cluster
// lookup and the owner request, which was also marked retryable. Mutation
// per site: restore the flattening there.
func TestFlatteningSitesReturnTheClassifiedError(t *testing.T) {
	srv := untrustedTokenServer(t)
	client := wrappedTokenClient(t, srv, tokenSlotRemedy)

	t.Run("postTokenCreate", func(t *testing.T) {
		_, _, err := postTokenCreate(t.Context(), client, srv.URL+ExtTokensPath, "ext/token-x:secret", map[string]any{}, TokenKindExt, "desc", "creating the token")
		_ = requireTypedTrust(t, err)
	})
	t.Run("pluginGetter", func(t *testing.T) {
		_, err := pluginGetter(client, srv.URL, "ext/token-x:secret")(t.Context(), steve.Request{Type: "provisioning.cattle.io.clusters"})
		_ = requireTypedTrust(t, err)
	})
	t.Run("requestOwner", func(t *testing.T) {
		_, err := requestOwner(t.Context(), client, srv.URL, "c-1", "ext/token-x:secret")
		_ = requireTypedTrust(t, err)
	})
}

// TestSignInOwnerCheckKeepsTheClassifiedCode asserts the owner check of an
// interactive sign-in, when its owner request fails the certificate check,
// keeps TLS_TRUST and not retryable, names the check, and carries the token
// fate as details. Mutation: copy the code from nothing (always GENERIC) or
// mark the error retryable.
func TestSignInOwnerCheckKeepsTheClassifiedCode(t *testing.T) {
	srv := untrustedTokenServer(t)
	client := wrappedTokenClient(t, srv, tokenSlotRemedy)
	ref := TokenRef{Kind: TokenKindExt, Name: "token-x"}
	created := CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: &ref, Bearer: "ext/token-x:secret"}
	fresh := newFreshToken(created, client, srv.URL, created.Bearer)

	err := userCheck{userID: "user-a", clusterID: "c-1"}.checkSignInOwner(t.Context(), client, srv.URL, created, fresh)
	ce := requireTypedTrust(t, err)
	assert.Contains(t, ce.Message, "checking the signed-in user against --user-id 'user-a': ")
	_, ok := ce.Details.(directWaitDetails)
	assert.True(t, ok, "the token fate stays the details")
}

// TestRevokeRecordsTheTextOfAnUnwrappedClient asserts a revoke through the
// client that classifies certificate failures records the same text, and
// leaves the same stamp, as the client that does not: the revoke output
// and the stamps are unchanged. Mutations: record the classified message
// for the logout half, or for the delete half.
func TestRevokeRecordsTheTextOfAnUnwrappedClient(t *testing.T) {
	srv := untrustedTokenServer(t)
	wrapped := wrappedTokenClient(t, srv, tokenSlotRemedy)
	plain := plainTokenClient(t)
	for _, tc := range []struct {
		name   string
		ref    TokenRef
		bearer string
		halves int
	}{
		{"an ext token", TokenRef{Kind: TokenKindExt, Name: "token-x"}, "ext/token-x:secret", 1},
		{"a v3 token with its own bearer, logout and delete", TokenRef{Kind: TokenKindV3, Name: "token-x"}, "token-x:secret", 2},
		{"a v3 token with another bearer, delete only", TokenRef{Kind: TokenKindV3, Name: "token-x"}, "token-y:secret", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := revokeToken(t.Context(), plain, srv.URL, tc.ref, tc.bearer)
			got := revokeToken(t.Context(), wrapped, srv.URL, tc.ref, tc.bearer)
			require.Equal(t, ResultFailed, want.Result)
			require.Contains(t, want.Error, "tls: failed to verify certificate")
			assert.Equal(t, want.Error, got.Error)
			assert.Equal(t, want.Result, got.Result)
			assert.Equal(t, tc.halves, strings.Count(got.Error, `"`+srv.URL), "one quoted URL per request that failed")
			assert.NotContains(t, got.Error, "CA-SLOT")
		})
	}
}

// TestRetireStampsTheTextOfAnUnwrappedClient asserts the stamp a superseded
// token's failed retirement leaves on the entry is the same text whether the
// credential's client classifies certificate failures or not. Mutation: record
// the classified message in revokeToken's delete half.
func TestRetireStampsTheTextOfAnUnwrappedClient(t *testing.T) {
	srv := untrustedTokenServer(t)
	stamp := func(t *testing.T, client *http.Client) string {
		t.Helper()
		dir := t.TempDir()
		cfg := &config.Config{Servers: map[string]config.Server{"e": {URL: srv.URL, Provenance: clierr.ProvenanceExecLogin}}}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		old := config.SupersededToken{Ref: TokenRef{Kind: TokenKindExt, Name: "token-old"}, UserID: "user-a", SupersededAt: "2026-09-02T00:00:00Z"}
		require.NoError(t, config.UpdateServerEntry(t.Context(), dir, "e", func(s *config.Server) bool {
			s.SupersededTokens = []config.SupersededToken{old}
			return true
		}))
		fresh := LoginCredential{Token: "ext/token-new:secret", Provenance: ProvenanceDerived, UserID: "user-a", client: client, base: srv.URL}
		reloaded, err := config.Load(config.FilePath(dir))
		require.NoError(t, err)
		rep := RetireSuperseded(t.Context(), dir, "e", srv.URL, fresh, reloaded.Servers["e"].SupersededTokens)
		require.Len(t, rep.Failed, 1)
		after, err := config.Load(config.FilePath(dir))
		require.NoError(t, err)
		recs := after.Servers["e"].SupersededTokens
		require.Len(t, recs, 1)
		require.NotNil(t, recs[0].LastAttempt)
		return recs[0].LastAttempt.Error
	}
	want := stamp(t, plainTokenClient(t))
	require.Contains(t, want, "tls: failed to verify certificate")
	assert.Equal(t, want, stamp(t, wrappedTokenClient(t, srv, tokenSlotRemedy)))
}

// TestPluginSignInClassifiesACertificateFailure asserts the exec plugin's
// own sign-in (token reuse on, no stored login, a terminal), whose first
// request fails the certificate check, ends with TLS_TRUST, exit 10 and not
// retryable, with the login failure details around it. Mutation: leave the
// client of clientFor unwrapped.
func TestPluginSignInClassifiesACertificateFailure(t *testing.T) {
	keyring.MockInit()
	t.Setenv(execInfoEnv, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCACert, "")
	t.Setenv(config.EnvCredentialStore, "")
	srv := untrustedTokenServer(t)
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{CredentialStore: "file", TokenReuse: true, Servers: map[string]config.Server{}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	flags := map[string]string{"server": srv.URL, "cluster": "c-1", "name": "kube", "auth-provider": "localProvider"}
	prompter := ui.New(&bytes.Buffer{}, newLineReader("alice", "pw"), false, true)
	_, err := NewTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand).Execute(t.Context(), tin{s: flags}, prompter)

	ce := tlstrust.TypedError(err)
	require.NotNil(t, ce, "got %v", err)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.False(t, ce.Retryable)
	assert.Contains(t, ce.Message, " and pass it with --cacert (a kubeconfig the CLI wrote also carries the CA")
	d, ok := ce.Details.(clierr.LoginFailureDetails)
	require.True(t, ok, "the details are the login failure around the primary, got %T", ce.Details)
	assert.Equal(t, clierr.CodeTLSTrust, d.Primary.Code)
	_, ok = d.Primary.Details.(tlstrust.Details)
	assert.True(t, ok, "the primary keeps the certificate details")
}

// TestClientForWrapsAnInjectedClientWithoutChangingIt asserts an injected
// client that has a transport is returned as a copy whose transport
// classifies, leaving the injected client alone, and that a client with no
// transport is returned as it is, so a test client never reaches
// http.DefaultTransport through the wrapper. Mutations: return the injected
// client unwrapped; wrap its transport in place; wrap a nil transport.
func TestClientForWrapsAnInjectedClientWithoutChangingIt(t *testing.T) {
	srv := untrustedTokenServer(t)
	opts := httpx.Options{WrapTransport: tlstrust.WrapTransportFor(srv.URL, tokenSlotRemedy)}

	own := &http.Transport{}
	t.Cleanup(own.CloseIdleConnections)
	injected := &http.Client{Transport: own, Timeout: 5 * time.Second}
	got, err := tokenCmd{newClient: func(httpx.Options) (*http.Client, error) { return injected, nil }}.clientFor(opts)
	require.NoError(t, err)
	assert.NotSame(t, injected, got)
	assert.Same(t, http.RoundTripper(own), injected.Transport, "the injected client keeps its transport")
	_, err = got.Get(srv.URL)
	_ = requireTypedTrust(t, err)

	bare := &http.Client{}
	got, err = tokenCmd{newClient: func(httpx.Options) (*http.Client, error) { return bare, nil }}.clientFor(opts)
	require.NoError(t, err)
	assert.Same(t, bare, got)
	assert.Nil(t, got.Transport)
}
