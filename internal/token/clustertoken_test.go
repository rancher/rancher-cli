package token

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func facadeAndBearer(t *testing.T, opts testrancher.Options) (*testrancher.Facade, *httptest.Server, string) {
	t.Helper()
	f := testrancher.New(opts)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cred, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{Server: srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0,
		ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
	require.NoError(t, err)
	return f, srv, cred.Token
}

// TestCreateClusterTokenExtHappyPath asserts the ext create carries
// spec.clusterName and the description, the response's scope is validated,
// and the result names the token with its owner.
func TestCreateClusterTokenExtHappyPath(t *testing.T) {
	t.Parallel()
	f, srv, bearer := facadeAndBearer(t, testrancher.Options{})
	r, err := createClusterToken(t.Context(), srv.Client(), srv.URL, bearer, "c-test", TokenDescription("h", "abcdef012345"))
	require.NoError(t, err)
	assert.Equal(t, CreateKnown, r.Outcome)
	assert.Equal(t, TokenKindExt, r.Kind)
	assert.Equal(t, "c-test", r.ClusterName)
	assert.Equal(t, "user-e2e", r.UserID)
	assert.True(t, r.Marked)
	last := f.CreateRequests()[len(f.CreateRequests())-1]
	assert.Equal(t, "cluster", last.Role)
	assert.Equal(t, bearer, last.Bearer)
	assert.Contains(t, last.Description, "[op:abcdef012345]")
}

// TestCreateClusterTokenFailsClosedOnScope asserts an ext response whose
// spec.clusterName is empty, missing, or another cluster is a create
// failure carrying the created token's ref so the caller revokes it; a 2xx
// with neither bearer nor name is unidentified with no ref.
func TestCreateClusterTokenFailsClosedOnScope(t *testing.T) {
	t.Parallel()
	empty, other := "", "c-other"
	cases := []struct {
		name string
		opts testrancher.Options
	}{
		{"empty clusterName", testrancher.Options{ScopedCreateClusterName: &empty}},
		{"missing spec", testrancher.Options{OmitSpecOnCreate: true}},
		{"other cluster", testrancher.Options{ScopedCreateClusterName: &other}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, srv, bearer := facadeAndBearer(t, tc.opts)
			r, err := createClusterToken(t.Context(), srv.Client(), srv.URL, bearer, "c-test", "d")
			require.ErrorIs(t, err, errClusterTokenUnscoped)
			assert.Equal(t, CreateKnown, r.Outcome)
			require.NotNil(t, r.Ref, "the created token is named so the caller can revoke it")
			assert.Empty(t, r.Bearer, "an unscoped bearer is never returned")
		})
	}
	t.Run("unidentified", func(t *testing.T) {
		t.Parallel()
		// A hand-rolled server: the facade's malformed option would break
		// the derived create that acquires the bearer too.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == ExtTokensPath {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"broken":true}`))
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)
		r, err := createClusterToken(t.Context(), srv.Client(), srv.URL, "ext/any:bearer", "c-test", "d")
		require.Error(t, err)
		assert.Equal(t, CreateUnidentified, r.Outcome)
		assert.Nil(t, r.Ref)
	})
}

// TestCreateClusterTokenV3FallbackContract asserts the legacy chain: ext
// 404 → POST /v3/tokens with clusterId, the response validated
// (non-empty token, clusterId equal to the request), owner read from
// userId; every failure row: 401/403/404 notCreated with the sentinels,
// 5xx unknown with no ref, an unscoped answer fails closed with the ref.
func TestCreateClusterTokenV3FallbackContract(t *testing.T) {
	t.Parallel()
	t.Run("happy", func(t *testing.T) {
		t.Parallel()
		f, srv, bearer := facadeAndBearer(t, testrancher.Options{Legacy: true, V3TokenCreate: true, LegacyLoginBodyUserID: true})
		r, err := createClusterToken(t.Context(), srv.Client(), srv.URL, bearer, "c-test", "d")
		require.NoError(t, err)
		assert.Equal(t, TokenKindV3, r.Kind)
		assert.Equal(t, "c-test", r.ClusterName)
		assert.Equal(t, "user-e2e", r.UserID)
		assert.Contains(t, r.Bearer, ":")
		last := f.CreateRequests()[len(f.CreateRequests())-1]
		assert.Equal(t, "v3", last.Kind)
	})
	t.Run("server too old", func(t *testing.T) {
		t.Parallel()
		_, srv, bearer := facadeAndBearer(t, testrancher.Options{Legacy: true})
		r, err := createClusterToken(t.Context(), srv.Client(), srv.URL, bearer, "c-test", "d")
		require.ErrorIs(t, err, errServerTooOldForReuse)
		assert.Equal(t, CreateNotCreated, r.Outcome)
	})
	for _, tc := range []struct {
		status int
		want   error
	}{{http.StatusUnauthorized, errCredentialUnauthorized}, {http.StatusForbidden, errCredentialForbidden}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			_, srv, bearer := facadeAndBearer(t, testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateStatus: tc.status})
			r, err := createClusterToken(t.Context(), srv.Client(), srv.URL, bearer, "c-test", "d")
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, CreateNotCreated, r.Outcome)
		})
	}
	t.Run("5xx is unknown", func(t *testing.T) {
		t.Parallel()
		_, srv, bearer := facadeAndBearer(t, testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateStatus: http.StatusServiceUnavailable})
		r, err := createClusterToken(t.Context(), srv.Client(), srv.URL, bearer, "c-test", "d")
		require.Error(t, err)
		assert.Equal(t, CreateUnknown, r.Outcome)
		assert.Nil(t, r.Ref)
	})
	t.Run("unscoped answer fails closed", func(t *testing.T) {
		t.Parallel()
		empty := ""
		_, srv, bearer := facadeAndBearer(t, testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateClusterID: &empty})
		r, err := createClusterToken(t.Context(), srv.Client(), srv.URL, bearer, "c-test", "d")
		require.ErrorIs(t, err, errClusterTokenUnscoped)
		assert.Equal(t, CreateKnown, r.Outcome)
		require.NotNil(t, r.Ref)
		assert.Empty(t, r.Bearer)
	})
}
