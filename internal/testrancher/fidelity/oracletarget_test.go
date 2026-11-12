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

func newTestOracleTarget(srv *httptest.Server) *OracleTarget {
	return &OracleTarget{baseURL: srv.URL, admin: "admin-token", client: srv.Client()}
}

func TestDeleteStandardUserSurfacesLookupFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	err := newTestOracleTarget(srv).deleteStandardUser(t.Context())
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

	err := newTestOracleTarget(srv).deleteStandardUser(t.Context())
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

	err := newTestOracleTarget(srv).deleteStandardUser(t.Context())
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

	tgt := newTestOracleTarget(srv)
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

	tgt := newTestOracleTarget(srv)
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

func TestNewOracleTargetFromEnv(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))

	t.Run("missing env vars error", func(t *testing.T) {
		for _, missing := range []string{"E2E_FIDELITY_URL", "E2E_FIDELITY_ADMIN_TOKEN", "E2E_FIDELITY_CACERT"} {
			t.Setenv("E2E_FIDELITY_URL", "https://rancher.test/")
			t.Setenv("E2E_FIDELITY_ADMIN_TOKEN", "admin-tok")
			t.Setenv("E2E_FIDELITY_CACERT", caPEM)
			t.Setenv(missing, "")
			_, err := NewOracleTargetFromEnv()
			require.Error(t, err, "missing %s must fail", missing)
		}
	})

	t.Run("unparseable CA errors", func(t *testing.T) {
		t.Setenv("E2E_FIDELITY_URL", "https://rancher.test/")
		t.Setenv("E2E_FIDELITY_ADMIN_TOKEN", "admin-tok")
		t.Setenv("E2E_FIDELITY_CACERT", "-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----")
		_, err := NewOracleTargetFromEnv()
		require.Error(t, err)
		assert.ErrorContains(t, err, "could not read any CA certificate from the PEM text")
	})

	t.Run("success wires the target", func(t *testing.T) {
		t.Setenv("E2E_FIDELITY_URL", "https://rancher.test/")
		t.Setenv("E2E_FIDELITY_ADMIN_TOKEN", "admin-tok")
		t.Setenv("E2E_FIDELITY_CACERT", caPEM)
		tgt, err := NewOracleTargetFromEnv()
		require.NoError(t, err)
		assert.Equal(t, "https://rancher.test", tgt.BaseURL(), "the trailing slash is trimmed")
		require.NotNil(t, tgt.Client())
		bearer, err := tgt.Bearer(t.Context(), IdentityAdmin)
		require.NoError(t, err)
		assert.Equal(t, "admin-tok", bearer)
		assert.Empty(t, tgt.Downstream(), "E2E_FIDELITY_DOWNSTREAM is unset")
		assert.Empty(t, tgt.PairInstallUUID(), "E2E_FIDELITY_PAIR_INSTALL_UUID is unset")
	})

	t.Run("E2E_FIDELITY_DOWNSTREAM names the downstream cluster", func(t *testing.T) {
		t.Setenv("E2E_FIDELITY_URL", "https://rancher.test/")
		t.Setenv("E2E_FIDELITY_ADMIN_TOKEN", "admin-tok")
		t.Setenv("E2E_FIDELITY_CACERT", caPEM)
		t.Setenv("E2E_FIDELITY_DOWNSTREAM", "c-abc123")
		t.Setenv("E2E_FIDELITY_PAIR_INSTALL_UUID", "5d3c1c8e-0000-4000-8000-000000000002")
		tgt, err := NewOracleTargetFromEnv()
		require.NoError(t, err)
		assert.Equal(t, "c-abc123", tgt.Downstream())
		assert.Equal(t, "5d3c1c8e-0000-4000-8000-000000000002", tgt.PairInstallUUID(), "E2E_FIDELITY_PAIR_INSTALL_UUID names the pair's install id")
	})

	t.Run("E2E_FIDELITY_CACERT=system trusts the OS root store, not a self-signed test cert", func(t *testing.T) {
		t.Setenv("E2E_FIDELITY_URL", srv.URL)
		t.Setenv("E2E_FIDELITY_ADMIN_TOKEN", "admin-tok")
		t.Setenv("E2E_FIDELITY_CACERT", "system")
		tgt, err := NewOracleTargetFromEnv()
		require.NoError(t, err, "the literal \"system\" must never be parsed as PEM")
		assert.Equal(t, srv.URL, tgt.BaseURL())

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		require.NoError(t, err)
		_, err = tgt.Client().Do(req)
		require.Error(t, err, "the test server's self-signed cert must be rejected: system roots, not the pinned CA, are trusted")
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

	tgt := newTestOracleTarget(srv)
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

	tgt := newTestOracleTarget(srv)
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

	bearer, err := newTestOracleTarget(srv).login(t.Context(), "u", "p")
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
		_, err := newTestOracleTarget(srv).login(t.Context(), "u", "p")
		require.Error(t, err)
		assert.ErrorContains(t, err, "status 401")
	})

	t.Run("transport error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()
		_, err := newTestOracleTarget(srv).login(t.Context(), "u", "p")
		require.Error(t, err)
	})

	t.Run("no token in response", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"foo": "bar"})
		}))
		defer srv.Close()
		_, err := newTestOracleTarget(srv).login(t.Context(), "u", "p")
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
	require.NoError(t, newTestOracleTarget(srv).deleteStandardUser(t.Context()))
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
	err := newTestOracleTarget(srv).deleteStandardUser(t.Context())
	require.ErrorContains(t, err, "still listed 300ms after delete")
}

// TestCleanupAttemptsEveryIdentityAndReportsEachFailure asserts the
// teardown is exhaustive: the standard user's delete fails, the member is
// still looked up and deleted, and the member's binding wait runs to its
// bound; the returned error names both users and their failing phases, and
// no binding wait is spent on the user whose delete failed.
func TestCleanupAttemptsEveryIdentityAndReportsEachFailure(t *testing.T) {
	oldBindings := bindingsGoneTimeout
	bindingsGoneTimeout = 200 * time.Millisecond
	t.Cleanup(func() { bindingsGoneTimeout = oldBindings })
	var memberDeleted atomic.Bool
	var memberDeletes, standardBindingLookups atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("username") {
		case standardUsername:
			_, _ = w.Write([]byte(`{"data":[{"id":"u-std","username":"e2e-fid-standard"}]}`))
		case memberUsername:
			if memberDeleted.Load() {
				_, _ = w.Write([]byte(`{"data":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"u-mem","username":"e2e-fid-member"}]}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	mux.HandleFunc("DELETE /v3/users/u-std", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("DELETE /v3/users/u-mem", func(w http.ResponseWriter, _ *http.Request) {
		memberDeletes.Add(1)
		memberDeleted.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	bindings := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("userId") == "u-std" {
			standardBindingLookups.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"lingering"}]}`))
	}
	mux.HandleFunc("GET /v3/globalrolebindings", bindings)
	mux.HandleFunc("GET /v3/projectroletemplatebindings", bindings)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	err := newTestOracleTarget(srv).Cleanup(t.Context())
	require.Error(t, err)
	assert.Equal(t, int32(1), memberDeletes.Load(), "the member must be deleted although the standard user's delete failed")
	require.ErrorContains(t, err, "cleaning up e2e-fid-standard: delete: deleting stale user u-std: status 500")
	require.ErrorContains(t, err, "cleaning up e2e-fid-member: bindings of u-mem: 2 binding(s) of deleted user u-mem still present after 200ms")
	assert.Equal(t, int32(0), standardBindingLookups.Load(), "no binding wait for a user the server refused to delete")
}

// TestCleanupWaitsForTheBindingsOfWhatWasDeletedWhenALaterDeleteFails
// asserts the binding waits follow the ids the delete removed: two users
// carry the reserved name, the first DELETE is accepted and the second
// refused; both binding collections are polled for the removed id, none
// for the refused one, and the delete error is joined with the wait's.
func TestCleanupWaitsForTheBindingsOfWhatWasDeletedWhenALaterDeleteFails(t *testing.T) {
	oldBindings := bindingsGoneTimeout
	bindingsGoneTimeout = 200 * time.Millisecond
	t.Cleanup(func() { bindingsGoneTimeout = oldBindings })
	var grbPolls, prtbPolls, refusedPolls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("username") == memberUsername {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"u-gone","username":"e2e-fid-standard"},{"id":"u-kept","username":"e2e-fid-standard"}]}`))
	})
	mux.HandleFunc("DELETE /v3/users/u-gone", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("DELETE /v3/users/u-kept", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) })
	bindings := func(counter *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("userId") {
			case "u-gone":
				counter.Add(1)
			case "u-kept":
				refusedPolls.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"lingering"}]}`))
		}
	}
	mux.HandleFunc("GET /v3/globalrolebindings", bindings(&grbPolls))
	mux.HandleFunc("GET /v3/projectroletemplatebindings", bindings(&prtbPolls))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	err := newTestOracleTarget(srv).Cleanup(t.Context())
	require.ErrorContains(t, err, "cleaning up e2e-fid-standard: delete: deleting stale user u-kept: status 409")
	require.ErrorContains(t, err, "bindings of u-gone: 2 binding(s) of deleted user u-gone still present after 200ms")
	assert.Positive(t, grbPolls.Load(), "the removed user's GlobalRoleBindings must be polled")
	assert.Positive(t, prtbPolls.Load(), "the removed user's ProjectRoleTemplateBindings must be polled")
	assert.Equal(t, int32(0), refusedPolls.Load(), "no binding wait for the user the server refused to delete")
}

// TestCleanupReportsATransientLookupWithoutDeletingOrPolling asserts a
// failed username lookup is reported as the delete phase's error and
// nothing else is attempted for that identity, while the other identity
// is still cleaned.
func TestCleanupReportsATransientLookupWithoutDeletingOrPolling(t *testing.T) {
	t.Parallel()
	var deletes, polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("username") == standardUsername {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	mux.HandleFunc("DELETE /v3/users/", func(w http.ResponseWriter, _ *http.Request) { deletes.Add(1); w.WriteHeader(http.StatusOK) })
	poll := func(w http.ResponseWriter, _ *http.Request) {
		polls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}
	mux.HandleFunc("GET /v3/globalrolebindings", poll)
	mux.HandleFunc("GET /v3/projectroletemplatebindings", poll)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	err := newTestOracleTarget(srv).Cleanup(t.Context())
	require.ErrorContains(t, err, "cleaning up e2e-fid-standard: delete: looking up user e2e-fid-standard: status 503")
	assert.NotContains(t, err.Error(), "e2e-fid-member", "the member's clean pass adds no error")
	assert.Equal(t, int32(0), deletes.Load())
	assert.Equal(t, int32(0), polls.Load())
}

// TestCleanupIsCleanWhenNoFixtureUserExists asserts the teardown is
// idempotent: neither reserved username is listed, nothing is deleted, no
// error.
func TestCleanupIsCleanWhenNoFixtureUserExists(t *testing.T) {
	t.Parallel()
	var deletes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/users", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	mux.HandleFunc("DELETE /v3/users/", func(w http.ResponseWriter, _ *http.Request) {
		deletes.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	require.NoError(t, newTestOracleTarget(srv).Cleanup(t.Context()))
	assert.Equal(t, int32(0), deletes.Load())
}

// TestCleanupDeletesEveryStaleCopyWhenAnEarlierDeleteIsRefused asserts
// the delete loop is exhaustive in the other order: two users carry the
// reserved name, the first DELETE is refused and the second accepted;
// both DELETEs are attempted, only the accepted id's bindings are polled,
// and the refused id is named in the joined error.
func TestCleanupDeletesEveryStaleCopyWhenAnEarlierDeleteIsRefused(t *testing.T) {
	oldBindings := bindingsGoneTimeout
	bindingsGoneTimeout = 200 * time.Millisecond
	t.Cleanup(func() { bindingsGoneTimeout = oldBindings })
	var refusedDeletes, acceptedDeletes, acceptedPolls, refusedPolls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("username") == memberUsername {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"u-kept","username":"e2e-fid-standard"},{"id":"u-gone","username":"e2e-fid-standard"}]}`))
	})
	mux.HandleFunc("DELETE /v3/users/u-kept", func(w http.ResponseWriter, _ *http.Request) {
		refusedDeletes.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("DELETE /v3/users/u-gone", func(w http.ResponseWriter, _ *http.Request) {
		acceptedDeletes.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	poll := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("userId") {
		case "u-gone":
			acceptedPolls.Add(1)
		case "u-kept":
			refusedPolls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}
	mux.HandleFunc("GET /v3/globalrolebindings", poll)
	mux.HandleFunc("GET /v3/projectroletemplatebindings", poll)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	err := newTestOracleTarget(srv).Cleanup(t.Context())
	require.ErrorContains(t, err, "cleaning up e2e-fid-standard: delete: deleting stale user u-kept: status 500")
	assert.Equal(t, int32(1), refusedDeletes.Load())
	assert.Equal(t, int32(1), acceptedDeletes.Load(), "the later copy must be deleted although the earlier DELETE was refused")
	assert.Equal(t, int32(2), acceptedPolls.Load(), "both binding collections polled once for the removed id (bindings already gone)")
	assert.Equal(t, int32(0), refusedPolls.Load(), "no binding wait for the copy the server refused to delete")
}

// TestDeleteStandardUserJoinsEveryRefusedCopy asserts the standard-user
// reset attempts every stale copy and reports each refusal.
func TestDeleteStandardUserJoinsEveryRefusedCopy(t *testing.T) {
	t.Parallel()
	var deletes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/users", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"u-a"},{"id":"u-b"},{"noid":true}]}`))
	})
	mux.HandleFunc("DELETE /v3/users/", func(w http.ResponseWriter, _ *http.Request) {
		deletes.Add(1)
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	err := newTestOracleTarget(srv).deleteStandardUser(t.Context())
	require.ErrorContains(t, err, "deleting stale user u-a: status 403")
	require.ErrorContains(t, err, "deleting stale user u-b: status 403")
	require.ErrorContains(t, err, "collection entry without id")
	assert.Equal(t, int32(2), deletes.Load(), "every id gets its DELETE attempt")
}
