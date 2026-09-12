package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewCompletionClientBoundsUnresponsiveServer asserts newCompletionClient
// itself bounds requests via its Options.Timeout, independent of any
// ctx deadline the caller happens to add. The GET is issued with a
// bare, deadline-free context - the same shape a caller that forgot to
// wrap ctx with completionTimeout() would use - so the client's own
// timeout is the only thing that can stop this from hanging forever.
func TestNewCompletionClientBoundsUnresponsiveServer(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })

	hc, err := newCompletionClient("")
	require.NoError(t, err)
	start := time.Now()
	p, err := completion.ManagementPath("management.cattle.io", "v3", "clusters")
	require.NoError(t, err)
	var dst any
	err = completion.NewConnection(hc, "token-abc:secret", srv.URL, false, 1).GetJSON(t.Context(), p, "", &dst)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 5*time.Second, "newCompletionClient must bound the request even with a deadline-free ctx")
}
