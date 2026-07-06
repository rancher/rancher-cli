package token

import (
	"context"
	"encoding/json"
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

	t.Run("named delete uses the full compound key", func(t *testing.T) {
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
