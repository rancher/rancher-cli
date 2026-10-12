package configcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/rancher/rancher-cli/internal/ui"
)

func v3LoginOn(t *testing.T, srv *httptest.Server) token.LoginCredential {
	t.Helper()
	body, err := json.Marshal(map[string]string{"type": "localProvider", "username": "e2e-user", "password": "e2e-pass", "responseType": "json"})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1-public/login", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, raw, err := httpx.Do(srv.Client(), req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var out struct {
		ID        string `json:"id"`
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	return token.LoginCredential{Token: out.Token, ExpiresAt: out.ExpiresAt, Provenance: token.ProvenanceLegacySession, UserID: "user-e2e",
		Ref: &token.TokenRef{Kind: token.TokenKindV3, Name: out.ID}}
}

// TestDeleteServerDeletesAnUnscopedV3TokenAndTheV3Login asserts config
// delete-server deletes an unscoped v3 cache token and a v3 login with their
// own bearers, removes the entry, and reports a cluster-scoped v3 cache token
// as not revoked, manual, with the administrator's delete.
func TestDeleteServerDeletesAnUnscopedV3TokenAndTheV3Login(t *testing.T) {
	keyring.MockInit()
	t.Setenv(config.EnvCredentialStore, "")
	f := testrancher.New(testrancher.Options{V3DeleteForbiddenForNonAdmin: true})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	login, unscoped, scoped := v3LoginOn(t, srv), v3LoginOn(t, srv), v3LoginOn(t, srv)
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]config.Server{
		"prod": {URL: srv.URL, Provenance: clierr.ProvenanceLegacySession, TokenRef: login.Ref, TokenUserID: login.UserID, CacheBindingID: "p1"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: srv.URL}, login.Token))
	for _, rec := range []struct {
		key     string
		cred    token.LoginCredential
		cluster string
	}{{"unscoped", unscoped, ""}, {"scoped", scoped, "c-1"}} {
		b := config.CacheBindingFor(config.CanonicalDir(dir), cfg, "prod").WithCreatedToken(rec.cred.Ref, rec.cred.UserID)
		require.NoError(t, store.SetCachedCred(rec.key, rec.cred.Token, time.Now().Add(time.Hour), "prod", config.CanonicalServerIdentity(srv.URL), rec.cluster, b))
	}

	res, err := NewConfigDeleteServer(dir).(*configDeleteServerCmd).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	revoked := data["revoked"].([]token.RevokedToken)
	require.Len(t, revoked, 2)
	assert.Equal(t, unscoped.Ref.Name, revoked[0].Ref.Name)
	assert.Equal(t, login.Ref.Name, revoked[1].Ref.Name)
	notRevoked := data["notRevoked"].([]token.NotRevokedToken)
	require.Len(t, notRevoked, 1)
	assert.Equal(t, scoped.Ref.Name, notRevoked[0].Ref.Name)
	assert.Equal(t, "v3", notRevoked[0].Reason)
	assert.Equal(t, "manual", notRevoked[0].Retry)
	assert.Equal(t, []string{v3DeleteCommand(scoped.Ref.Name)}, notRevoked[0].Recovery)
	require.NotEmpty(t, res.Warnings)
	assert.True(t, strings.HasPrefix(res.Warnings[0], "the cluster token "+scoped.Ref.String()+" was not revoked: the CLI cannot delete this v3 token; "), res.Warnings[0])
	after, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := after.Servers["prod"]
	assert.False(t, ok, "the entry is removed")
	for _, c := range []token.LoginCredential{login, unscoped} {
		_, alive := f.TokenOwner(c.Ref.Name)
		assert.False(t, alive, c.Ref.Name)
	}
	_, alive := f.TokenOwner(scoped.Ref.Name)
	assert.True(t, alive)
}
