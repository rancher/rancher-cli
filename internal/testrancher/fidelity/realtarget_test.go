package fidelity

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/token"
)

func newTestRealTarget(srv *httptest.Server) *RealTarget {
	return &RealTarget{baseURL: srv.URL, admin: "admin-token", client: srv.Client()}
}

func TestDeleteStandardUserSurfacesLookupFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	err := newTestRealTarget(srv).deleteStandardUser(t.Context())
	require.Error(t, err)
	assert.ErrorContains(t, err, "status 403", "an HTTP lookup failure must never read as already-clean")
}

func TestDeleteStandardUserSurfacesMalformed200(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"unexpected":"shape"}`))
	}))
	defer srv.Close()

	err := newTestRealTarget(srv).deleteStandardUser(t.Context())
	require.Error(t, err)
	assert.ErrorContains(t, err, "no data array")
}

func TestDeleteStandardUserSurfacesDeleteFailure(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "u-stale"}}})
	})
	mux.HandleFunc("DELETE /v3/users/u-stale", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	err := newTestRealTarget(srv).deleteStandardUser(t.Context())
	require.Error(t, err)
	require.ErrorContains(t, err, "u-stale")
	assert.ErrorContains(t, err, "status 403")
}

func TestDoJSONRedactsCredentialBodiesInErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"bearerToken": "ext/leak:sentinel-sekrit"},
		})
	}))
	defer srv.Close()

	tgt := newTestRealTarget(srv)
	_, _, err := tgt.adminJSON(t.Context(), http.MethodPost, "/v3/tokens", map[string]any{}, http.StatusCreated)
	require.Error(t, err, "status 200 != want 201 must error")
	assert.NotContains(t, err.Error(), "sentinel-sekrit",
		"a token created under an unexpected status is still live; the error must redact it")
	assert.ErrorContains(t, err, "<secret>", "the redacted shape stays visible for diagnosis")
}

func TestStandardBearerDeletesBeforeCreateThenLogsIn(t *testing.T) {
	t.Parallel()
	var order []string
	mux := http.NewServeMux()
	deleted := false
	mux.HandleFunc("GET /v3/users", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "lookup")
		w.Header().Set("Content-Type", "application/json")
		if deleted { // the second lookup confirms the stale user is gone
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "u-stale"}}})
	})
	mux.HandleFunc("DELETE /v3/users/u-stale", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "delete")
		deleted = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /v3/users", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "create")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"u-new"}`))
	})
	mux.HandleFunc("POST /v3/globalrolebindings", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "grb")
		var grb struct {
			GlobalRoleID string `json:"globalRoleId"`
			UserID       string `json:"userId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&grb)
		assert.Equal(t, "user", grb.GlobalRoleID, "the baseline user role is bound")
		assert.Equal(t, "u-new", grb.UserID, "the binding targets the just-created user")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /v1-public/login", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "login")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "ext.cattle.io/v1", "kind": "Token",
			"status": map[string]any{"bearerToken": "ext/std-1:sec-1"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tgt := newTestRealTarget(srv)
	bearer, err := tgt.Bearer(t.Context(), IdentityStandard)
	require.NoError(t, err)
	assert.Equal(t, "ext/std-1:sec-1", bearer)
	assert.Equal(t, []string{"lookup", "delete", "lookup", "create", "grb", "login"}, order,
		"the stale user is removed and confirmed gone, the new user is created and bound to the baseline role, then logs in")

	again, err := tgt.Bearer(t.Context(), IdentityStandard)
	require.NoError(t, err)
	assert.Equal(t, bearer, again, "the standard bearer is memoized")
	assert.Len(t, order, 6, "memoization must not re-provision")
}

func TestNewRealTargetFromEnv(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))

	t.Run("missing env vars error", func(t *testing.T) {
		for _, missing := range []string{"E2E_REAL_URL", "E2E_REAL_ADMIN_TOKEN", "E2E_REAL_CACERT"} {
			t.Setenv("E2E_REAL_URL", "https://rancher.test/")
			t.Setenv("E2E_REAL_ADMIN_TOKEN", "admin-tok")
			t.Setenv("E2E_REAL_CACERT", caPEM)
			t.Setenv(missing, "")
			_, err := NewRealTargetFromEnv()
			require.Error(t, err, "missing %s must fail", missing)
		}
	})

	t.Run("unparseable CA errors", func(t *testing.T) {
		t.Setenv("E2E_REAL_URL", "https://rancher.test/")
		t.Setenv("E2E_REAL_ADMIN_TOKEN", "admin-tok")
		t.Setenv("E2E_REAL_CACERT", "-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----")
		_, err := NewRealTargetFromEnv()
		require.Error(t, err)
		assert.ErrorContains(t, err, "failed to append CA certs from PEM")
	})

	t.Run("success wires the target", func(t *testing.T) {
		t.Setenv("E2E_REAL_URL", "https://rancher.test/")
		t.Setenv("E2E_REAL_ADMIN_TOKEN", "admin-tok")
		t.Setenv("E2E_REAL_CACERT", caPEM)
		tgt, err := NewRealTargetFromEnv()
		require.NoError(t, err)
		assert.Equal(t, "https://rancher.test", tgt.BaseURL(), "the trailing slash is trimmed")
		require.NotNil(t, tgt.Client())
		bearer, err := tgt.Bearer(t.Context(), IdentityAdmin)
		require.NoError(t, err)
		assert.Equal(t, "admin-tok", bearer)
	})
}

func TestRedactBodyScrubsAndSummarizes(t *testing.T) {
	t.Parallel()
	assert.Contains(t, redactBody([]byte("this is not json")), "non-JSON body",
		"a non-JSON body is summarized, never echoed")

	scrubbed := redactBody([]byte(`{"status":{"bearerToken":"ext/x:secret"},"metadata":{"resourceVersion":"9"}}`))
	assert.Contains(t, scrubbed, "<secret>", "the bearer is scrubbed before it can enter an error message")
	assert.NotContains(t, scrubbed, "ext/x", "the raw bearer never appears")
	assert.Contains(t, scrubbed, "<rv>", "volatile fields are normalized too")
}

func TestJSONMarshalNoEscapeSurfacesEncodeError(t *testing.T) {
	t.Parallel()
	_, err := jsonMarshalNoEscape(map[string]any{"bad": make(chan int)})
	require.Error(t, err,
		"an unmarshalable value surfaces the encoder error behind redactBody's defensive unrenderable branch")
}

func TestRevokedBearerCreatesThenRevokesByDerivedName(t *testing.T) {
	t.Parallel()
	var deletedPath string
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+token.ExtTokensPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"bearerToken": "ext/tok-rev:sec-rev"},
		})
	})
	mux.HandleFunc("DELETE "+token.ExtTokensPath+"/tok-rev", func(w http.ResponseWriter, r *http.Request) {
		deletedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tgt := newTestRealTarget(srv)
	bearer, err := tgt.Bearer(t.Context(), IdentityRevoked)
	require.NoError(t, err)
	assert.Equal(t, "ext/tok-rev:sec-rev", bearer, "the revoked bearer is the created (now-deleted) credential")
	assert.Equal(t, token.ExtTokensPath+"/tok-rev", deletedPath,
		"the delete targets the name embedded in the ext bearer, not a metadata.name")

	again, err := tgt.Bearer(t.Context(), IdentityRevoked)
	require.NoError(t, err)
	assert.Equal(t, bearer, again, "the revoked bearer is memoized")
}

func TestRevokedBearerNonExtCreateErrorsWithoutLeaking(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"bearerToken": "plain-secret-xyz"},
		})
	}))
	defer srv.Close()

	tgt := newTestRealTarget(srv)
	_, err := tgt.Bearer(t.Context(), IdentityRevoked)
	require.Error(t, err)
	require.ErrorContains(t, err, "not ext-shaped")
	assert.NotContains(t, err.Error(), "plain-secret-xyz",
		"a create response bearer of the wrong shape is still a live secret; the error withholds it")
}

func TestLoginAcceptsNormanShape(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "norman-tok"})
	}))
	defer srv.Close()

	bearer, err := newTestRealTarget(srv).login(t.Context(), "u", "p")
	require.NoError(t, err)
	assert.Equal(t, "norman-tok", bearer, "the Norman {token} shape is accepted as a fallback")
}

func TestLoginSurfacesNon2xxAndTransportAndMissingToken(t *testing.T) {
	t.Parallel()

	t.Run("non-2xx", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		_, err := newTestRealTarget(srv).login(t.Context(), "u", "p")
		require.Error(t, err)
		assert.ErrorContains(t, err, "status 401")
	})

	t.Run("transport error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()
		_, err := newTestRealTarget(srv).login(t.Context(), "u", "p")
		require.Error(t, err)
	})

	t.Run("no token in response", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"foo": "bar"})
		}))
		defer srv.Close()
		_, err := newTestRealTarget(srv).login(t.Context(), "u", "p")
		require.Error(t, err)
		assert.ErrorContains(t, err, "no token in response")
	})
}

// TestDeleteStandardUserWaitsForAsynchronousDeletion asserts the delete
// waits for the username lookup to stop returning the user: the server
// answers the lookup with the user once more after DELETE 200, then empty,
// and the call returns nil only after that second lookup.
func TestDeleteStandardUserWaitsForAsynchronousDeletion(t *testing.T) {
	var lookups, deletes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/users", func(w http.ResponseWriter, r *http.Request) {
		n := lookups.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n <= 2 { // before the delete, and once more right after it
			_, _ = w.Write([]byte(`{"data":[{"id":"u-stale","username":"e2e-fid-standard"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	mux.HandleFunc("DELETE /v3/users/u-stale", func(w http.ResponseWriter, _ *http.Request) {
		deletes.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	require.NoError(t, newTestRealTarget(srv).deleteStandardUser(t.Context()))
	assert.Equal(t, int32(1), deletes.Load())
	assert.Equal(t, int32(3), lookups.Load(), "one lookup before the delete, two until the user is gone")
}

// TestDeleteStandardUserGivesUpWhenTheUserNeverLeaves asserts the wait is
// bounded and names the bound.
func TestDeleteStandardUserGivesUpWhenTheUserNeverLeaves(t *testing.T) {
	old := standardUserGoneTimeout
	standardUserGoneTimeout = 300 * time.Millisecond
	t.Cleanup(func() { standardUserGoneTimeout = old })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/users", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"u-stale","username":"e2e-fid-standard"}]}`))
	})
	mux.HandleFunc("DELETE /v3/users/u-stale", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	err := newTestRealTarget(srv).deleteStandardUser(t.Context())
	require.ErrorContains(t, err, "still listed 300ms after delete")
}
