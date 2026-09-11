package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// roleServer serves a management API with mixed-context roletemplates and
// globalroles, counting hits.
func roleServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/roletemplates", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"project-owner"},"context":"project"},
			{"metadata":{"name":"project-member"},"context":"project"},
			{"metadata":{"name":"project-member"},"context":"project"},
			{"metadata":{"name":"cluster-owner"},"context":"cluster"},
			{"metadata":{"name":"bad name!"},"context":"project"},
			{"metadata":{"name":"no-context-role"}}
		]}`))
	})
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/globalroles", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"admin"}},
			{"metadata":{"name":"user"}}
		]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestFetchRoleTemplatesFiltersContext(t *testing.T) {
	t.Parallel()
	srv, _ := roleServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	got := fetchRoleTemplates(t.Context(), srv.Client(), cap, "project")
	// project-context only; "bad name!" sanitized out; duplicate deduped;
	// cluster-context and context-less rows excluded; sorted.
	assert.Equal(t, []string{"project-member", "project-owner"}, got)

	got = fetchRoleTemplates(t.Context(), srv.Client(), cap, "cluster")
	assert.Equal(t, []string{"cluster-owner"}, got)
}

func TestFetchGlobalRoles(t *testing.T) {
	t.Parallel()
	srv, _ := roleServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	got := fetchGlobalRoles(t.Context(), srv.Client(), cap)
	assert.Equal(t, []string{"admin", "user"}, got)
}

func TestFetchRolesTokenBoundary(t *testing.T) {
	t.Parallel()
	srv, hits := roleServer(t)
	tests := []struct {
		name string
		cap  TokenCapability
	}{
		{name: "no token", cap: TokenCapability{ServerURL: srv.URL}},
		{name: "IDOnly exec-cred", cap: TokenCapability{Token: "downstream", IDOnly: true, ServerURL: srv.URL}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Nil(t, fetchRoleTemplates(t.Context(), srv.Client(), tt.cap, "project"))
			assert.Nil(t, fetchGlobalRoles(t.Context(), srv.Client(), tt.cap))
		})
	}
	assert.Zero(t, hits.Load(), "token boundary must return nil WITHOUT a request")
}

func TestFetchRolesFailEmpty(t *testing.T) {
	t.Parallel()
	// 401 and malformed JSON both yield nil.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	assert.Nil(t, fetchRoleTemplates(t.Context(), srv.Client(), cap, "project"))
	assert.Nil(t, fetchGlobalRoles(t.Context(), srv.Client(), cap))

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	t.Cleanup(srv2.Close)
	cap2 := TokenCapability{Token: "token-abc:secret", ServerURL: srv2.URL}
	require.Nil(t, fetchRoleTemplates(t.Context(), srv2.Client(), cap2, "project"))
	require.Nil(t, fetchGlobalRoles(t.Context(), srv2.Client(), cap2))
}

func TestFetchRolesTimeout(t *testing.T) {
	t.Parallel()
	// The deadline case: a server slower than the context deadline
	// yields nil, silently.
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-block
		}))
		t.Cleanup(func() { close(block) })
		client := srv.Client() // sets srv.URL: an in-memory server's URL is set by its first Client call
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		assert.Nil(t, fetchRoleTemplates(ctx, client, cap, "project"))
		assert.Nil(t, fetchGlobalRoles(ctx, client, cap))
	})
}
