package testtransport

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

// These tests swap http.DefaultTransport for the whole test binary, so
// none of them runs in parallel and the package starts no other server.

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// requireNoGuard fails a test that starts while an earlier test left the
// guard installed.
func requireNoGuard(t *testing.T) {
	t.Helper()
	_, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok, "http.DefaultTransport is not the standard transport: an earlier test left the guard installed")
}

// installGuard installs the guard for a test that asserts the error of
// restore itself, and restores it when the test ends even if an assertion
// stops the test first.
func installGuard(t *testing.T) (restore func() error) {
	t.Helper()
	requireNoGuard(t)
	restore = GuardDefaultTransport()
	t.Cleanup(func() { _ = restore() })
	return restore
}

// installQuietGuard installs the guard for a test that expects no request to
// reach it: when the test ends, restore must report none.
func installQuietGuard(t *testing.T) (restore func() error) {
	t.Helper()
	requireNoGuard(t)
	restore = GuardDefaultTransport()
	t.Cleanup(func() { assert.NoError(t, restore()) })
	return restore
}

// listed is the number of URLs restore is specified to list.
const listed = 20

// closeRecorder is a request body that records Close.
type closeRecorder struct {
	io.Reader
	closed atomic.Bool
}

func (c *closeRecorder) Close() error {
	c.closed.Store(true)
	return nil
}

// TestGuardFailsADefaultClientRequest asserts that a request through
// http.DefaultClient fails with the guard's error, that the URL appears in
// that error once, and that restore reports the request's URL.
func TestGuardFailsADefaultClientRequest(t *testing.T) {
	srv := newServer(t)
	restore := installGuard(t)

	resp, err := http.DefaultClient.Get(srv.URL + "/ping")
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorContains(t, err, "http.DefaultTransport")
	require.ErrorContains(t, err, srv.URL+"/ping")
	assert.Equal(t, 1, strings.Count(err.Error(), srv.URL+"/ping"), "the URL is in the error once: %v", err)

	err = restore()
	require.ErrorContains(t, err, "used by 1 request(s)")
	require.ErrorContains(t, err, srv.URL+"/ping")
}

// TestGuardFailsAPlainHTTPRestConfig asserts that client-go, which hands a
// plain-HTTP rest.Config the default transport, reaches the guard too, and
// that a config carrying the server's Transport does not.
func TestGuardFailsAPlainHTTPRestConfig(t *testing.T) {
	srv := newServer(t)
	restore := installGuard(t)

	plain, err := rest.HTTPClientFor(&rest.Config{Host: srv.URL, BearerToken: "t"})
	require.NoError(t, err)
	resp, err := plain.Get(srv.URL + "/plain")
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorContains(t, err, "http.DefaultTransport")

	own, err := rest.HTTPClientFor(&rest.Config{Host: srv.URL, BearerToken: "t", Transport: srv.Client().Transport})
	require.NoError(t, err)
	resp, err = own.Get(srv.URL + "/own")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	err = restore()
	require.ErrorContains(t, err, srv.URL+"/plain")
	assert.NotContains(t, err.Error(), "/own")
}

// TestGuardLetsAServerClientThrough asserts that a request through the
// server's own client succeeds while the guard is installed and leaves
// restore with nothing to report.
func TestGuardLetsAServerClientThrough(t *testing.T) {
	srv := newServer(t)
	installQuietGuard(t)

	resp, err := srv.Client().Get(srv.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

// TestGuardRestoresTheOriginalTransport asserts that restore puts back the
// exact transport it replaced, and that the guard is a value client-go can
// compare with http.DefaultTransport.
func TestGuardRestoresTheOriginalTransport(t *testing.T) {
	original := http.DefaultTransport
	restore := installQuietGuard(t)
	assert.NotSame(t, original, http.DefaultTransport)
	guarded := http.DefaultTransport
	assert.NotPanics(t, func() { _ = guarded == http.DefaultTransport })
	require.NoError(t, restore())
	assert.Same(t, original, http.DefaultTransport)
}

// TestGuardClosesTheRequestBody asserts that a request the guard fails has
// its body closed: http.Client leaves that to the transport.
func TestGuardClosesTheRequestBody(t *testing.T) {
	srv := newServer(t)
	installGuard(t)

	body := &closeRecorder{Reader: strings.NewReader("payload")}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/body", body)
	require.NoError(t, err)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorContains(t, err, "http.DefaultTransport")
	assert.True(t, body.closed.Load(), "the guard closed the request body")
}

// TestGuardRecordsRequestsFromParallelGoroutines asserts that every
// request from concurrent goroutines is recorded, each URL once, up to the
// number restore lists.
func TestGuardRecordsRequestsFromParallelGoroutines(t *testing.T) {
	srv := newServer(t)
	restore := installGuard(t)

	var wg sync.WaitGroup
	for i := range listed {
		wg.Go(func() {
			resp, err := http.DefaultClient.Get(srv.URL + "/n/" + string(rune('a'+i)))
			if resp != nil {
				_ = resp.Body.Close()
			}
			assert.Error(t, err)
		})
	}
	wg.Wait()

	err := restore()
	require.Error(t, err)
	require.ErrorContains(t, err, fmt.Sprintf("used by %d request(s)", listed))
	assert.NotContains(t, err.Error(), "more;")
	for i := range listed {
		require.ErrorContains(t, err, srv.URL+"/n/"+string(rune('a'+i)))
	}
}

// TestGuardListsAtMostTheFirstURLs asserts that restore gives the total
// count, lists the first URLs in arrival order and counts the rest.
func TestGuardListsAtMostTheFirstURLs(t *testing.T) {
	srv := newServer(t)
	restore := installGuard(t)

	const total = listed + 5
	for i := range total {
		resp, err := http.DefaultClient.Get(fmt.Sprintf("%s/seq/%02d", srv.URL, i))
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.Error(t, err)
	}

	err := restore()
	require.ErrorContains(t, err, fmt.Sprintf("used by %d request(s)", total))
	require.ErrorContains(t, err, " and 5 more;")
	for i := range listed {
		require.ErrorContains(t, err, fmt.Sprintf("%s/seq/%02d", srv.URL, i))
	}
	for i := listed; i < total; i++ {
		assert.NotContains(t, err.Error(), fmt.Sprintf("%s/seq/%02d", srv.URL, i))
	}
}
