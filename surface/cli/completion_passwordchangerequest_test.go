package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// userListServer serves the management User list, counting hits.
func userListServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/users", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"u-abc123"},"username":"alice"},
			{"metadata":{"name":"u-def456"},"username":"bob"}
		]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestLivePositionalPasswordChangeRequestUserCandidates asserts
// bespokeSingleKindOps' create-password-change-request entry: `create
// password-change-request <TAB>` lists existing user ids through the same
// generic management-kind path get/patch/delete user uses — no per-command
// completer needed.
func TestLivePositionalPasswordChangeRequestUserCandidates(t *testing.T) {
	t.Parallel()
	srv, hits := userListServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "create password-change-request"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
	assert.Equal(t, []string{"u-abc123", "u-def456"}, got)
	assert.Equal(t, int32(1), hits.Load(), "exactly one management list request")
}

// TestPositionalCreateUserUsernameHasNoLiveCompletion proves create user's
// <username> positional deliberately gets no live completion — it names a
// NEW username, never an existing one.
func TestPositionalCreateUserUsernameHasNoLiveCompletion(t *testing.T) {
	t.Parallel()
	srv, hits := userListServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "create user"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
	assert.Nil(t, got)
	assert.Zero(t, hits.Load(), "no request for a positional that names a new value")
}
