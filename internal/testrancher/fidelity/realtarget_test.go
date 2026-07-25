package fidelity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	err := newTestRealTarget(srv).deleteStandardUser(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 403", "an HTTP lookup failure must never read as already-clean")
}

func TestDeleteStandardUserSurfacesMalformed200(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"unexpected":"shape"}`))
	}))
	defer srv.Close()

	err := newTestRealTarget(srv).deleteStandardUser(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no data array")
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

	err := newTestRealTarget(srv).deleteStandardUser(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "u-stale")
	assert.Contains(t, err.Error(), "status 403")
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
	_, _, err := tgt.adminJSON(context.Background(), http.MethodPost, "/v3/tokens", map[string]any{}, http.StatusCreated)
	require.Error(t, err, "status 200 != want 201 must error")
	assert.NotContains(t, err.Error(), "sentinel-sekrit",
		"a token minted under an unexpected status is still live; the error must redact it")
	assert.Contains(t, err.Error(), "<secret>", "the redacted shape stays visible for diagnosis")
}

func TestStandardBearerDeletesBeforeCreateThenLogsIn(t *testing.T) {
	t.Parallel()
	var order []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v3/users", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "lookup")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "u-stale"}}})
	})
	mux.HandleFunc("DELETE /v3/users/u-stale", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "delete")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /v3/users", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "create")
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
	bearer, err := tgt.Bearer(context.Background(), IdentityStandard)
	require.NoError(t, err)
	assert.Equal(t, "ext/std-1:sec-1", bearer)
	assert.Equal(t, []string{"lookup", "delete", "create", "login"}, order,
		"the stale user is removed before the create, then the new user logs in")

	again, err := tgt.Bearer(context.Background(), IdentityStandard)
	require.NoError(t, err)
	assert.Equal(t, bearer, again, "the standard bearer is memoized")
	assert.Len(t, order, 4, "memoization must not re-provision")
}
