package provider

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rancherVersionPath = "/rancherversion"

// newVersionFake starts a TLS fake standing in for Rancher's unauthenticated
// /rancherversion endpoint. Any path other than /rancherversion 404s,
// mirroring the requirement that the probe never falls back to a different path.
func newVersionFake(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func effFor(t *testing.T, srv *httptest.Server) config.Effective {
	t.Helper()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return config.Effective{URL: srv.URL, CA: string(certPEM), Token: "id:secret"}
}

func TestRancherVersionReturnsParsedVersion(t *testing.T) {
	t.Parallel()
	srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != rancherVersionPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"v2.15.0","GitCommit":"abc123","RancherPrime":"false"}`))
	})

	got, err := RancherVersion(t.Context(), effFor(t, srv))
	require.NoError(t, err)
	assert.Equal(t, "v2.15.0", got)
}

// TestRancherVersionDevBuildReturnsUnparseable verifies that a dev-stamped
// version string ("v2.15.0-dev") is returned as-is so the caller's
// EvalMinVersionSupport can report it verbatim in the payload.
func TestRancherVersionDevBuildReturnsUnparseable(t *testing.T) {
	t.Parallel()
	srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != rancherVersionPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"v2.15.0-dev","GitCommit":"deadbeef"}`))
	})

	got, err := RancherVersion(t.Context(), effFor(t, srv))
	require.NoError(t, err)
	// Caller (EvalMinVersionSupport) decides whether this is parseable.
	assert.Equal(t, "v2.15.0-dev", got)
}

// TestRancherVersionGarbageBodyReturnsError pins the 200-but-unrecognized-body
// case: a non-Rancher server (or a proxy) that returns 200 with an unexpected
// payload must be treated as a read failure so the caller can degrade to
// UnknownServerVersionWarning.
func TestRancherVersionGarbageBodyReturnsError(t *testing.T) {
	t.Parallel()

	t.Run("invalid JSON", func(t *testing.T) {
		t.Parallel()
		srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(`not json at all`))
		})
		_, err := RancherVersion(t.Context(), effFor(t, srv))
		require.Error(t, err)
		assert.ErrorContains(t, err, "/rancherversion")
	})

	t.Run("valid JSON but no Version field", func(t *testing.T) {
		t.Parallel()
		srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"something":"else"}`))
		})
		_, err := RancherVersion(t.Context(), effFor(t, srv))
		require.Error(t, err)
		assert.ErrorContains(t, err, "no Version field")
	})
}

// TestRancherVersionNon200ReturnsError pins the non-200 path: any status other
// than 200 (e.g. 404 from a non-Rancher server, 503 during startup) is a read
// failure — never silently treated as an unknown version.
func TestRancherVersionNon200ReturnsError(t *testing.T) {
	t.Parallel()
	srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, err := RancherVersion(t.Context(), effFor(t, srv))
	require.Error(t, err)
	assert.ErrorContains(t, err, "503")
}

// TestRancherVersionUnreachableReturnsError pins the connectivity-failure path:
// when the server is unreachable (connection refused after close), the error
// propagates so checkMinRancherVersion can degrade to UnknownServerVersionWarning.
func TestRancherVersionUnreachableReturnsError(t *testing.T) {
	t.Parallel()
	srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"v2.15.0"}`))
	})
	eff := effFor(t, srv)
	srv.Close() // close before the call; t.Cleanup will call Close again (idempotent)

	_, err := RancherVersion(t.Context(), eff)
	require.Error(t, err)
}

// TestRancherVersionPathValuedCA pins that a file-path CA (the common case
// after `rancher auth login --cacert /path/to/ca.pem`) is resolved through
// config.ResolveCA before being passed to httpx.New, so the TLS handshake
// succeeds rather than failing to parse a filesystem path as raw PEM.
func TestRancherVersionPathValuedCA(t *testing.T) {
	t.Parallel()
	srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != rancherVersionPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"v2.15.0"}`))
	})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, certPEM, 0600))

	eff := config.Effective{URL: srv.URL, CA: caFile, Token: "id:secret"}
	got, err := RancherVersion(t.Context(), eff)
	require.NoError(t, err)
	assert.Equal(t, "v2.15.0", got)
}

// TestGetServerInfoDecodesEdition pins RancherPrime's string-wire decode
// (not a JSON boolean): "true"/"false" (any case/whitespace) decode to their
// Edition; absent or garbage decode to EditionUnknown — never treated as
// community.
func TestGetServerInfoDecodesEdition(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want Edition
	}{
		{"true", `{"Version":"v2.15.0","RancherPrime":"true"}`, EditionPrime},
		{"false", `{"Version":"v2.15.0","RancherPrime":"false"}`, EditionCommunity},
		{"mixed case and padding", `{"Version":"v2.15.0","RancherPrime":" True "}`, EditionPrime},
		{"absent", `{"Version":"v2.15.0"}`, EditionUnknown},
		{"garbage", `{"Version":"v2.15.0","RancherPrime":"yes"}`, EditionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			})
			info, err := GetServerInfo(t.Context(), effFor(t, srv))
			require.NoError(t, err)
			assert.Equal(t, "v2.15.0", info.Version)
			assert.Equal(t, tc.want, info.Edition)
		})
	}
}

// TestGetServerInfoPropagatesReadErrors pins that GetServerInfo shares
// RancherVersion's classified-failure behavior (malformed body, non-200,
// unreachable) rather than swallowing it into EditionUnknown — the
// capability detector's caller decides how to fold a probe error.
func TestGetServerInfoPropagatesReadErrors(t *testing.T) {
	t.Parallel()
	srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_, err := GetServerInfo(t.Context(), effFor(t, srv))
	require.Error(t, err)
}

// TestRancherVersionTrailingSlashURL pins that a trailing slash in eff.URL
// (set via RANCHER_CLI_URL env, which bypasses canonicalization) is trimmed
// before appending "/rancherversion", so the probe does not send a double-
// slash request that some server routers reject with 404.
func TestRancherVersionTrailingSlashURL(t *testing.T) {
	t.Parallel()
	srv := newVersionFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != rancherVersionPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"v2.15.0"}`))
	})
	eff := effFor(t, srv)
	eff.URL += "/" // trailing slash, as set by RANCHER_CLI_URL env verbatim
	got, err := RancherVersion(t.Context(), eff)
	require.NoError(t, err)
	assert.Equal(t, "v2.15.0", got)
}
