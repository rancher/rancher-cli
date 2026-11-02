package auth

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
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/rancher/rancher-cli/internal/ui"
)

// v3LoginOn creates a live unscoped v3 token of e2e-user on srv, as a legacy
// sign-in does.
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

// v3Dir is a directory whose entry prod holds a v3 login the CLI created
// and named, with an unscoped and a cluster-scoped v3 cache record.
type v3Dir struct {
	dir                     string
	f                       *testrancher.Facade
	login, unscoped, scoped token.LoginCredential
}

func newV3Dir(t *testing.T) v3Dir {
	t.Helper()
	keyring.MockInit()
	t.Setenv(config.EnvCredentialStore, "")
	f := testrancher.New(testrancher.Options{V3DeleteForbiddenForNonAdmin: true})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	d := v3Dir{dir: t.TempDir(), f: f, login: v3LoginOn(t, srv), unscoped: v3LoginOn(t, srv), scoped: v3LoginOn(t, srv)}
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]config.Server{
		"prod": {URL: srv.URL, Provenance: clierr.ProvenanceLegacySession, TokenRef: d.login.Ref, TokenUserID: d.login.UserID,
			CredentialExpiresAt: d.login.ExpiresAt, CacheBindingID: "b1"},
	}}
	cfg.SetPath(config.FilePath(d.dir))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(d.dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: srv.URL}, d.login.Token))
	server := config.CanonicalServerIdentity(srv.URL)
	for _, rec := range []struct {
		key     string
		cred    token.LoginCredential
		cluster string
	}{{"unscoped", d.unscoped, ""}, {"scoped", d.scoped, "c-1"}} {
		b := config.CacheBindingFor(config.CanonicalDir(d.dir), cfg, "prod").WithCreatedToken(rec.cred.Ref, rec.cred.UserID)
		require.NoError(t, store.SetCachedCred(rec.key, rec.cred.Token, time.Now().Add(time.Hour), "prod", server, rec.cluster, b))
	}
	return d
}

// TestLogoutDeletesAnUnscopedV3TokenAndTheV3Login asserts auth logout
// deletes an unscoped v3 cache token and a v3 login with their own
// bearers, in that order, and reports a cluster-scoped v3 cache token as
// not revoked with the administrator's delete, in the data, the text and the
// warnings.
func TestLogoutDeletesAnUnscopedV3TokenAndTheV3Login(t *testing.T) {
	d := newV3Dir(t)
	res, err := NewLogout(d.dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	require.NoError(t, err)
	raw, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var data struct {
		Revoked    []token.RevokedToken    `json:"revoked"`
		NotRevoked []token.NotRevokedToken `json:"notRevoked"`
	}
	require.NoError(t, json.Unmarshal(raw, &data))
	assert.Equal(t, []string{d.unscoped.Ref.Name, d.login.Ref.Name}, revokedNames(data.Revoked))
	require.Len(t, data.NotRevoked, 1)
	n := data.NotRevoked[0]
	assert.Equal(t, d.scoped.Ref.Name, n.Ref.Name)
	assert.Equal(t, "v3", n.Reason)
	assert.Equal(t, "manual", n.Retry)
	assert.Equal(t, []string{clierr.Rancher + " delete management-token '" + d.scoped.Ref.Name + "' --apply --yes --allow-deprecated"}, n.Recovery)
	require.Len(t, res.Warnings, 1)
	assert.True(t, strings.HasPrefix(res.Warnings[0], "the cluster token "+d.scoped.Ref.String()+" was not revoked: the CLI cannot delete this v3 token; "), res.Warnings[0])
	assert.Contains(t, res.Warnings[0], "; an administrator can delete it sooner with: ")
	assert.NotContains(t, res.Warnings[0], clierr.RecoverySequenceAssumption)
	assert.Contains(t, res.Text, "  revoked "+d.unscoped.Ref.String()+" (cached token)\n")
	assert.Contains(t, res.Text, "  revoked "+d.login.Ref.String()+" (login token)")
	for _, c := range []token.LoginCredential{d.unscoped, d.login} {
		_, alive := d.f.TokenOwner(c.Ref.Name)
		assert.False(t, alive, c.Ref.Name)
	}
	_, alive := d.f.TokenOwner(d.scoped.Ref.Name)
	assert.True(t, alive, "Rancher refuses a cluster-scoped token on every path that names no cluster")
	cfg := mustLoad(t, d.dir)
	require.Len(t, cfg.Servers["prod"].SupersededTokens, 1, "the token not revoked stays recorded")
	assert.Equal(t, d.scoped.Ref.Name, cfg.Servers["prod"].SupersededTokens[0].Ref.Name)
}
