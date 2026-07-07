package token

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tin struct {
	s map[string]string
	b map[string]bool
	a []string
}

func (t tin) String(k string) string { return t.s[k] }
func (t tin) Bool(k string) bool     { return t.b[k] }
func (t tin) Args() []string         { return t.a }

func TestExecCredentialJSONShape(t *testing.T) {
	cred := newExecCredential("kubeconfig-u-abc:xyz", time.Time{})
	b, err := json.Marshal(cred)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "ExecCredential", got["kind"])
	assert.Equal(t, "client.authentication.k8s.io/v1beta1", got["apiVersion"])
	status := got["status"].(map[string]any)
	assert.Equal(t, "kubeconfig-u-abc:xyz", status["token"])
}

func TestDeleteTokenCommand(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cmd := NewDeleteTokenCommand(dir, store)

	t.Run("no name and no --all is validation error", func(t *testing.T) {
		_, err := cmd.Execute(context.Background(), tin{}, safety.Effect{}, ui.Discard())
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})

	t.Run("--all flushes the cache", func(t *testing.T) {
		require.NoError(t, store.SetCachedCred("acme/u/c", "tok", time.Now().Add(time.Hour)))
		_, err := cmd.Execute(context.Background(), tin{b: map[string]bool{"all": true}}, safety.Effect{}, ui.Discard())
		require.NoError(t, err)
		_, _, gerr := store.GetCachedCred("acme/u/c")
		assert.ErrorIs(t, gerr, config.ErrNoToken)
	})

	t.Run("named delete removes the entry", func(t *testing.T) {
		require.NoError(t, store.SetCachedCred("acme/u/c", "tok", time.Now().Add(time.Hour)))
		_, err := cmd.Execute(context.Background(), tin{a: []string{"acme/u/c"}}, safety.Effect{}, ui.Discard())
		require.NoError(t, err)
		_, _, gerr := store.GetCachedCred("acme/u/c")
		assert.ErrorIs(t, gerr, config.ErrNoToken)
	})

	t.Run("--all with a name is a validation error, not a silent flush", func(t *testing.T) {
		require.NoError(t, store.SetCachedCred("keep/u/c", "tok", time.Now().Add(time.Hour)))
		_, err := cmd.Execute(context.Background(), tin{b: map[string]bool{"all": true}, a: []string{"keep/u/c"}}, safety.Effect{}, ui.Discard())
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		// the cache must be untouched — nothing deleted
		_, _, gerr := store.GetCachedCred("keep/u/c")
		require.NoError(t, gerr)
	})
}

func TestBasicAuthNonInteractiveIsAuthError(t *testing.T) {
	t.Parallel()
	input := &loginInput{server: "https://test.example.com", authProvider: "localProvider"}
	_, err := basicAuth(context.Background(), nil, input, true, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
}

func TestExecCredentialOmitsEmptyStatusToken(t *testing.T) {
	// Embedding (not a json:",inline" tag) is what promotes kind/apiVersion.
	cred := newExecCredential("", time.Time{})
	b, err := json.Marshal(cred)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "ExecCredential", got["kind"])
	status := got["status"].(map[string]any)
	_, hasTok := status["token"]
	assert.False(t, hasTok, "empty token must be omitted, not emitted as empty string")
}

func TestResolveCA(t *testing.T) {
	t.Parallel()
	pem := "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"
	got, err := resolveCA(pem)
	require.NoError(t, err)
	assert.Equal(t, pem, got, "inline PEM passes through")

	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(path, []byte(pem), 0600))
	got, err = resolveCA(path)
	require.NoError(t, err)
	assert.Equal(t, pem, got, "path is read to PEM")

	got, err = resolveCA("")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestGetTokenServerRequired(t *testing.T) {
	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir))
	_, err := cmd.Execute(context.Background(), tin{}, safety.Effect{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestGetTokenCacheHit(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetCachedCred("prod", "cached-tok", time.Now().Add(time.Hour)))
	cmd := NewTokenCommand(dir, store)

	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": "https://r.ex", "name": "prod"}},
		safety.Effect{}, ui.Discard())
	require.NoError(t, err)
	assert.True(t, res.Raw)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "cached-tok", cred.Status.Token)
}

func TestSelectAuthProviderExplicitMatch(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&LocalProvider{AuthProvider{Type: "localProvider"}}, &AuthProvider{Type: "pingProvider"}}
	got, err := selectAuthProvider(context.Background(), ps, "pingProvider", ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "pingProvider", got.GetType())
}

func TestSelectAuthProviderNotFound(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider"}}
	_, err := selectAuthProvider(context.Background(), ps, "nope", ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, candidateIDs(ce.Candidates), "pingProvider")
}

func TestSelectAuthProviderSingle(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider"}}
	got, err := selectAuthProvider(context.Background(), ps, "", ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "pingProvider", got.GetType())
}

func TestSelectAuthProviderAmbiguousNoInput(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider"}, &AuthProvider{Type: "oktaProvider"}}
	_, err := selectAuthProvider(context.Background(), ps, "", ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.ElementsMatch(t, []string{"pingProvider", "oktaProvider"}, candidateIDs(ce.Candidates))
}

func candidateIDs(cs []clierr.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

// lineReader yields one line at a time without buffering, so multiple readLine
// calls (each creating a fresh Scanner) don't lose data to Scanner's internal buffer.
type lineReader struct {
	lines []string
	pos   int
}

func newLineReader(lines ...string) *lineReader {
	return &lineReader{lines: lines}
}

func (r *lineReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.lines) {
		return 0, io.EOF
	}
	line := r.lines[r.pos] + "\n"
	r.pos++
	n := copy(p, line)
	return n, nil
}

func TestGetTokenSAMLNonInteractiveIsAuthError(t *testing.T) {
	// A SAML provider under a non-interactive prompter must fast-fail AUTH(3),
	// not open a browser and poll for 15 minutes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"pingProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir))
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
		safety.Effect{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
}

func TestResolveConnConfigFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	caPEM := "-----BEGIN CERTIFICATE-----\nMIICertData\n-----END CERTIFICATE-----\n"
	cfg := &config.Config{
		CurrentContext: "acme",
		Contexts:       map[string]config.Context{"acme": {Server: "acme", Cluster: "local"}},
		Servers:        map[string]config.Server{"acme": {URL: "https://config.example", CA: caPEM}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := tokenCmd{dir: dir, store: store}

	t.Run("no flags sources from config", func(t *testing.T) {
		server, ca, cluster, err := cmd.resolveConn(tin{})
		require.NoError(t, err)
		assert.Equal(t, "https://config.example", server)
		assert.Equal(t, "local", cluster)
		assert.Equal(t, caPEM, ca, "inline PEM passes through resolveCA")
	})

	t.Run("flag override while config fills remaining", func(t *testing.T) {
		server, ca, cluster, err := cmd.resolveConn(tin{s: map[string]string{"server": "https://flag.example"}})
		require.NoError(t, err)
		assert.Equal(t, "https://flag.example", server)
		assert.Equal(t, "local", cluster, "cluster still from config")
		assert.Equal(t, caPEM, ca, "CA still from config")
	})
}

func TestGetTokenNoNameWritesCachingDisabledNotice(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	var errBuf strings.Builder
	p := ui.New(&errBuf, strings.NewReader(""), false, false)
	cmd := NewTokenCommand(dir, config.NewFileStore(dir))
	_, err := cmd.Execute(context.Background(), tin{s: map[string]string{"server": srv.URL}}, safety.Effect{}, p)
	require.Error(t, err, "non-interactive local auth should fail")
	assert.Contains(t, errBuf.String(), "caching disabled", "notice must appear in Err")
}

func TestBasicAuth401IsAuthError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))
			return
		}
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir))
	var errBuf strings.Builder
	p := ui.New(&errBuf, newLineReader("alice", "secret"), false, true)
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
		safety.Effect{}, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code, "401 from login POST must map to CodeAuth")
}

func TestOAuthDeviceCodeFlowNoInputIsAuthError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"azureADProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir))
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "auth-flow": "devicecode"}},
		safety.Effect{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code, "device-code flow with NoInput must fast-fail AUTH before network")
}

func TestOAuthDeviceCodeFlowNonTTYAllowed(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"azureADProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	var errBuf strings.Builder
	p := ui.New(&errBuf, strings.NewReader(""), false, false)
	cmd := NewTokenCommand(dir, config.NewFileStore(dir))
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "auth-flow": "devicecode"}},
		safety.Effect{}, p)
	require.Error(t, err, "device-code will fail on OAuth config/network, but not on the gate")
	assert.NotContains(t, err.Error(), "interactive login required", "non-TTY must bypass the gate, fail later")
}

func TestOAuthAuthCodeFlowNoInputIsAuthError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"azureADProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir))
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "auth-flow": "authcode"}},
		safety.Effect{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code, "authcode flow with NoInput must fast-fail AUTH before network")
}

func TestOAuthAuthCodeFlowNonTTYAllowed(t *testing.T) {
	// Not parallel: sets a process-wide env var. authcode, once past the gate,
	// opens a browser and waits ~5m on a loopback callback; forcing an invalid
	// callback port makes it fail fast right after the gate, so the test proves
	// the gate is bypassed for non-TTY without entering that wait.
	t.Setenv("CATTLE_OAUTH_CALLBACK_PORT", "70000") // out of range -> getCallbackPort errors
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"azureADProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	var errBuf strings.Builder
	p := ui.New(&errBuf, strings.NewReader(""), false, false)
	cmd := NewTokenCommand(dir, config.NewFileStore(dir))
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "auth-flow": "authcode"}},
		safety.Effect{}, p)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "interactive login required", "non-TTY must bypass the gate")
	assert.Contains(t, err.Error(), "callback port", "should fail past the gate on the forced port error")
}

func TestSelectAuthProviderCtrlCIsNotAmbiguous(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var errBuf strings.Builder
	p := ui.New(&errBuf, strings.NewReader(""), false, true)
	providers := []typedProvider{&AuthProvider{Type: "pingProvider"}, &AuthProvider{Type: "oktaProvider"}}
	_, err := selectAuthProvider(ctx, providers, "", p)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "cancelled context must return context.Canceled, not AMBIGUOUS_SELECTION")
}
