package provider

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
)

// storedLogin builds the Effective of a config directory in t.TempDir() whose
// current entry, prod, holds a stored token with the given provenance and
// auth config, for the server at url with the CA caPEM.
func storedLogin(t *testing.T, url, caPEM string, provenance clierr.Provenance, authConfig string) config.Effective {
	t.Helper()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: url}, "token-x:y"))
	cfg := &config.Config{
		CurrentServer: "prod",
		Servers: map[string]config.Server{"prod": {
			URL: url, CA: caPEM, Provenance: provenance, TokenAuthConfig: authConfig,
		}},
	}
	eff, err := cfg.Effective(store)
	require.NoError(t, err)
	require.Equal(t, "token-x:y", eff.Token)
	return eff
}

// TestRefusedStoredTokenNamesTheLoginCommand asserts a 401 to the token of a
// stored entry names the login command for that entry as its hint, through
// the error mapping of a call that takes the Effective (mapErr and
// mapPreflightErr), through the requests of this package that take it, and
// through the cluster client, in a read and in a pod log read: a pasted
// token names --token-stdin and a login through an auth config names
// --auth-provider with that auth config.
func TestRefusedStoredTokenNamesTheLoginCommand(t *testing.T) {
	t.Parallel()
	r := refusal{name: "401 empty", status: http.StatusUnauthorized, code: clierr.CodeAuth, noReason: true}
	for _, tc := range []struct {
		name       string
		provenance clierr.Provenance
		authConfig string
		want       string
	}{
		{"a pasted token", clierr.ProvenanceUserSupplied, "", clierr.Rancher + " auth login 'prod' --token-stdin"},
		{"a login through an auth config", clierr.ProvenanceDerived, "corp-ldap", clierr.Rancher + " auth login 'prod' --auth-provider 'corp-ldap'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, caPEM := refusingTLSServer(t, r)
			eff := storedLogin(t, srv.URL, caPEM, tc.provenance, tc.authConfig)
			unauthorized := clierr.ResponseError(http.MethodGet, http.StatusUnauthorized, nil)

			hintOf := func(label string, err error) {
				t.Helper()
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%s: got %v", label, err)
				assert.Equal(t, clierr.CodeAuth, ce.Code, label)
				assert.Equal(t, tc.want, ce.Hint, label)
			}
			hintOf("mapErr", mapErr(t.Context(), http.MethodGet, eff, unauthorized))
			hintOf("mapPreflightErr", mapPreflightErr(t.Context(), http.MethodGet, eff, unauthorized))
			for name, call := range providerCalls {
				hintOf(name, call(t.Context(), eff))
			}

			c, err := NewClusterClient(&rest.Config{Host: srv.URL, BearerToken: "t", Transport: srv.Client().Transport}, "local", WithLogin(eff.LoginRemedy))
			require.NoError(t, err)
			gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
			_, err = c.Get(t.Context(), gvr, "", "x")
			hintOf("cluster client read", err)
			_, err = c.Logs(t.Context(), "default", "x", engine.LogOptions{})
			hintOf("cluster client pod log read", err)
			res, err := c.PostReview(t.Context(), engine.ReviewRequest{Kind: engine.ReviewSelfSubjectIdentity, Object: reviewObj("SelfSubjectReview")})
			require.NoError(t, err)
			hintOf("cluster client review", res.Mapped)
		})
	}
}
