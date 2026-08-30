package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestNewCompletionClientBoundsUnresponsiveServer asserts newCompletionClient
// itself bounds requests via its Options.Timeout, independent of any
// ctx deadline the caller happens to add. FetchClustersForTest is called
// with a bare, deadline-free context — the same shape a caller that forgot
// to wrap ctx with completionTimeout() would use — so the client's own
// timeout is the only thing that can stop this from hanging forever.
func TestNewCompletionClientBoundsUnresponsiveServer(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })

	cap := TokenCapabilityForTest(srv.URL, "token-abc:secret", false)
	start := time.Now()
	got := FetchClustersForTest(t.Context(), cap)
	elapsed := time.Since(start)

	assert.Nil(t, got)
	assert.Less(t, elapsed, 5*time.Second, "newCompletionClient must bound the request even with a deadline-free ctx")
}
