package completion

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listServer answers every GET with a two-item list and records the hits,
// the last Authorization and Accept headers, and the last request URI.
type listServer struct {
	srv    *httptest.Server
	hits   atomic.Int32
	auth   atomic.Pointer[string]
	accept atomic.Pointer[string]
	uri    atomic.Pointer[string]
}

func newListServer(t *testing.T) *listServer {
	t.Helper()
	ls := &listServer{}
	ls.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.hits.Add(1)
		a, acc, u := r.Header.Get("Authorization"), r.Header.Get("Accept"), r.RequestURI
		ls.auth.Store(&a)
		ls.accept.Store(&acc)
		ls.uri.Store(&u)
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"b"}},{"metadata":{"name":"a"}}]}`))
	}))
	t.Cleanup(ls.srv.Close)
	return ls
}

// TestGetJSONSendsBearerAcceptAndPath asserts one GET carries the bearer,
// the default Accept header and the built path, and decodes the envelope.
func TestGetJSONSendsBearerAcceptAndPath(t *testing.T) {
	t.Parallel()
	ls := newListServer(t)
	conn := NewConnection(ls.srv.Client(), "tok-secret", ls.srv.URL, false, 1)
	var list ItemList
	p := must(ManagementPath("management.cattle.io", "v3", "globalroles"))
	require.NoError(t, conn.GetJSON(t.Context(), p, "", &list))
	assert.Equal(t, []string{"b", "a"}, list.Names())
	assert.Equal(t, "Bearer tok-secret", *ls.auth.Load())
	assert.Equal(t, "application/json", *ls.accept.Load())
	assert.Equal(t, "/k8s/clusters/local/apis/management.cattle.io/v3/globalroles", *ls.uri.Load())
	assert.Equal(t, 1, conn.Used())
	assert.False(t, conn.Exceeded())
	assert.False(t, conn.IDOnly())
}

// TestGetJSONCustomAcceptAndNoToken asserts a caller-chosen Accept header
// is sent as given and an empty token sends no Authorization header.
func TestGetJSONCustomAcceptAndNoToken(t *testing.T) {
	t.Parallel()
	ls := newListServer(t)
	conn := NewConnection(ls.srv.Client(), "", ls.srv.URL, true, 1)
	var list ItemList
	p := must(ClusterPath("c-x", "", "v1", "namespaces"))
	require.NoError(t, conn.GetJSON(t.Context(), p, "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList", &list))
	assert.Equal(t, "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList", *ls.accept.Load())
	assert.Empty(t, *ls.auth.Load())
	assert.True(t, conn.IDOnly())
}

// TestGetJSONAllowanceIsEnforcedAndSticky asserts the request past the
// grant is refused without sending, the refusal is remembered, and a
// later request within what would have been the grant is refused too.
func TestGetJSONAllowanceIsEnforcedAndSticky(t *testing.T) {
	t.Parallel()
	ls := newListServer(t)
	conn := NewConnection(ls.srv.Client(), "tok", ls.srv.URL, false, 2)
	p := must(ManagementPath("management.cattle.io", "v3", "roletemplates"))
	var list ItemList
	require.NoError(t, conn.GetJSON(t.Context(), p, "", &list))
	require.NoError(t, conn.GetJSON(t.Context(), p, "", &list))
	assert.False(t, conn.Exceeded())
	err := conn.GetJSON(t.Context(), p, "", &list)
	require.ErrorIs(t, err, ErrAllowanceExceeded)
	assert.True(t, conn.Exceeded())
	assert.Equal(t, int32(2), ls.hits.Load(), "the refused request is never sent")
	err = conn.GetJSON(t.Context(), p, "", &list)
	require.ErrorIs(t, err, ErrAllowanceExceeded)
	assert.Equal(t, int32(2), ls.hits.Load())
	assert.Equal(t, 2, conn.Used())
}

// TestSetTraceReportsEveryRequestSent asserts the trace hook sees the
// path, status, body size and a transport error, never a refused request,
// and that a nil hook is silence.
func TestSetTraceReportsEveryRequestSent(t *testing.T) {
	t.Parallel()
	type line struct {
		path   string
		status int
		size   int
		err    error
	}
	var lines []line
	ls := newListServer(t)
	conn := NewConnection(ls.srv.Client(), "tok", ls.srv.URL, false, 1)
	conn.SetTrace(func(path string, status, size int, elapsed time.Duration, err error) {
		assert.GreaterOrEqual(t, elapsed, time.Duration(0))
		lines = append(lines, line{path, status, size, err})
	})
	p := must(ManagementPath("management.cattle.io", "v3", "globalroles"))
	var list ItemList
	require.NoError(t, conn.GetJSON(t.Context(), p, "", &list))
	require.ErrorIs(t, conn.GetJSON(t.Context(), p, "", &list), ErrAllowanceExceeded)
	require.Len(t, lines, 1, "a refused request is not sent and not traced")
	assert.Equal(t, "/k8s/clusters/local/apis/management.cattle.io/v3/globalroles", lines[0].path)
	assert.Equal(t, http.StatusOK, lines[0].status)
	assert.Positive(t, lines[0].size)
	require.NoError(t, lines[0].err)

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	failing := NewConnection(closed.Client(), "tok", closed.URL, false, 1)
	var got line
	failing.SetTrace(func(path string, status, size int, _ time.Duration, err error) { got = line{path, status, size, err} })
	require.Error(t, failing.GetJSON(t.Context(), p, "", &list))
	assert.Equal(t, p.String(), got.path)
	assert.Zero(t, got.status)
	assert.Zero(t, got.size)
	require.Error(t, got.err)

	silent := NewConnection(ls.srv.Client(), "tok", ls.srv.URL, false, 1)
	silent.SetTrace(nil)
	require.NoError(t, silent.GetJSON(t.Context(), p, "", &list))
}

// TestZeroConnectionFailsClosed asserts a Connection nobody constructed
// refuses every request, sends nothing, and reports itself exceeded.
func TestZeroConnectionFailsClosed(t *testing.T) {
	t.Parallel()
	var conn Connection
	p := must(ManagementPath("management.cattle.io", "v3", "users"))
	var list ItemList
	require.ErrorIs(t, conn.GetJSON(t.Context(), p, "", &list), ErrAllowanceExceeded)
	assert.True(t, conn.Exceeded())
	assert.Equal(t, 0, conn.Used())
}

// TestZeroLimitFailsClosed asserts a constructed Connection with no
// allowance refuses its first request.
func TestZeroLimitFailsClosed(t *testing.T) {
	t.Parallel()
	ls := newListServer(t)
	conn := NewConnection(ls.srv.Client(), "tok", ls.srv.URL, false, 0)
	var list ItemList
	p := must(ManagementPath("management.cattle.io", "v3", "users"))
	require.ErrorIs(t, conn.GetJSON(t.Context(), p, "", &list), ErrAllowanceExceeded)
	assert.True(t, conn.Exceeded())
	assert.Zero(t, ls.hits.Load())
}

// TestGetJSONEmptyPathIsRefusedUncounted asserts a zero Path, which no
// builder produced, is refused before anything is sent or counted.
func TestGetJSONEmptyPathIsRefusedUncounted(t *testing.T) {
	t.Parallel()
	ls := newListServer(t)
	conn := NewConnection(ls.srv.Client(), "tok", ls.srv.URL, false, 1)
	var list ItemList
	require.ErrorIs(t, conn.GetJSON(t.Context(), Path{}, "", &list), ErrEmptyPath)
	assert.Zero(t, ls.hits.Load())
	assert.Equal(t, 0, conn.Used())
	assert.False(t, conn.Exceeded())
}

// TestGetJSONErrors asserts a non-2xx status and an undecodable body are
// errors, and that each counted as a sent request.
func TestGetJSONErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{name: "401", handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, want: "HTTP 401"},
		{name: "500", handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, want: "HTTP 500"},
		{name: "not json", handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{not json`)) }, want: "invalid character"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(tt.handler)
			t.Cleanup(srv.Close)
			conn := NewConnection(srv.Client(), "tok", srv.URL, false, 1)
			var list ItemList
			p := must(ManagementPath("management.cattle.io", "v3", "users"))
			err := conn.GetJSON(t.Context(), p, "", &list)
			require.ErrorContains(t, err, tt.want)
			assert.Equal(t, 1, conn.Used())
			assert.False(t, conn.Exceeded())
		})
	}
}

// TestGetJSONHonorsTheDeadline asserts a server slower than the context
// deadline yields the context's error, on the fake clock.
func TestGetJSONHonorsTheDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-block
		}))
		t.Cleanup(func() { close(block) })
		client := srv.Client() // sets srv.URL: an in-memory server's URL is set by its first Client call
		conn := NewConnection(client, "tok", srv.URL, false, 1)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		var list ItemList
		p := must(ManagementPath("management.cattle.io", "v3", "users"))
		err := conn.GetJSON(ctx, p, "", &list)
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded))
		assert.Equal(t, 1, conn.Used())
	})
}

// TestListingPresets asserts the declarations sources share.
func TestListingPresets(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Requirements{Cluster: ClusterNone, Credential: CredentialManagement, Allowance: AllowanceOne}, ManagementListing())
	assert.Equal(t, Requirements{Cluster: ClusterNone, Credential: CredentialManagement, Allowance: AllowanceTwo}, ManagementTwoListings())
	assert.Equal(t, Requirements{Cluster: ClusterResolved, Credential: CredentialManagement, Allowance: AllowanceOne}, ClusterListing())
	assert.Equal(t, Requirements{Cluster: ClusterResolved, Credential: CredentialAny, Allowance: AllowanceThree}, ClusterThreeReads())
}
