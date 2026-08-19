package cli

import (
	"context"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// TestLivePositionalDeleteUserCandidates pins the management branch case for
// the user kind: `delete user <TAB>` lists management.cattle.io/v3 Users from
// the local cluster, excluding system accounts, matching the grant/revoke
// subject candidate source.
func TestLivePositionalDeleteUserCandidates(t *testing.T) {
	t.Parallel()
	srv, hits := userServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "delete user"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)

	assert.Contains(t, got, "u-alice", "user id must be a candidate")
	assert.NotContains(t, got, "alice", "username must not be offered for delete user — rejected verbatim")
	assert.NotContains(t, got, "u-sys", "system user id must be excluded")
	assert.NotContains(t, got, "system-svc", "system username must be excluded")
	assert.Equal(t, int32(1), hits.Load(), "exactly one management list request")
}

// TestLivePositionalDeleteUserTokenBoundary pins the management-token
// boundary: missing token or exec-cred-only (IDOnly) capability cannot
// authorize the management user list — nil, with ZERO requests.
func TestLivePositionalDeleteUserTokenBoundary(t *testing.T) {
	t.Parallel()
	srv, hits := userServer(t)
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "delete user"}
	for _, cap := range []TokenCapability{
		{ServerURL: srv.URL},
		{Token: "downstream", IDOnly: true, ServerURL: srv.URL},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
		cancel()
		assert.Nil(t, got)
	}
	assert.Zero(t, hits.Load(), "token boundary must return nil WITHOUT a request")
}

// TestLivePositionalGetUserCandidates confirms the management branch also
// handles `get user <TAB>` using the same user fetch (AC-10 includes get).
func TestLivePositionalGetUserCandidates(t *testing.T) {
	t.Parallel()
	srv, hits := userServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get user"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)

	assert.Contains(t, got, "u-alice")
	assert.NotContains(t, got, "u-sys")
	assert.Equal(t, int32(1), hits.Load())
}

// TestLivePositionalPatchUserCandidates confirms the management branch handles
// `patch user <TAB>` using the same user fetch as get and delete.
func TestLivePositionalPatchUserCandidates(t *testing.T) {
	t.Parallel()
	srv, hits := userServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "patch user"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)

	assert.Contains(t, got, "u-alice", "user id must be a candidate")
	assert.NotContains(t, got, "alice", "username must not be offered for patch user — rejected verbatim")
	assert.NotContains(t, got, "u-sys", "system user id must be excluded")
	assert.Equal(t, int32(1), hits.Load(), "exactly one management list request")
}
