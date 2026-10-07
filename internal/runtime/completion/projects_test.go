package completion

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bodyServer struct {
	srv    *httptest.Server
	hits   atomic.Int32
	auth   atomic.Pointer[string]
	status atomic.Int32
}

func newBodyServer(t *testing.T, status int, body string) *bodyServer {
	t.Helper()
	bs := &bodyServer{}
	bs.status.Store(int32(status))
	bs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs.hits.Add(1)
		a := r.Header.Get("Authorization")
		bs.auth.Store(&a)
		w.WriteHeader(int(bs.status.Load()))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(bs.srv.Close)
	return bs
}

func steveProjectsPath(t *testing.T) Path {
	t.Helper()
	return must(SteveNamespacedCollectionPath("management.cattle.io.projects", "c-abc"))
}

// TestGetBodyReturnsTheBodyAndSendsTheBearer asserts a 2xx answer returns
// its body unchanged, the bearer token is sent, and the request counts
// against the allowance.
func TestGetBodyReturnsTheBodyAndSendsTheBearer(t *testing.T) {
	t.Parallel()
	bs := newBodyServer(t, http.StatusOK, `{"data":[]}`)
	conn := NewConnection(bs.srv.Client(), "tok-secret", bs.srv.URL, false, 5)
	body, err := conn.GetBody(t.Context(), steveProjectsPath(t))
	require.NoError(t, err)
	assert.JSONEq(t, `{"data":[]}`, string(body))
	assert.Equal(t, "Bearer tok-secret", *bs.auth.Load())
	assert.Equal(t, 1, conn.Used())
}

// TestGetBodyNon2xxIsAStatusError asserts every non-2xx answer is a
// *StatusError with the status, and still counts against the allowance.
func TestGetBodyNon2xxIsAStatusError(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		bs := newBodyServer(t, status, "nope")
		conn := NewConnection(bs.srv.Client(), "tok", bs.srv.URL, false, 5)
		body, err := conn.GetBody(t.Context(), steveProjectsPath(t))
		require.Error(t, err)
		assert.Nil(t, body)
		se, ok := errors.AsType[*StatusError](err)
		require.True(t, ok, "status %d: %v", status, err)
		assert.Equal(t, status, se.Status)
		assert.Equal(t, 1, conn.Used())
	}
}

// TestGetBodyCountsAgainstTheAllowance asserts GetBody spends the allowance
// like GetJSON: with a limit of 1 the second call is refused without
// sending anything.
func TestGetBodyCountsAgainstTheAllowance(t *testing.T) {
	t.Parallel()
	bs := newBodyServer(t, http.StatusOK, `{}`)
	conn := NewConnection(bs.srv.Client(), "tok", bs.srv.URL, false, 1)
	_, err := conn.GetBody(t.Context(), steveProjectsPath(t))
	require.NoError(t, err)
	_, err = conn.GetBody(t.Context(), steveProjectsPath(t))
	require.ErrorIs(t, err, ErrAllowanceExceeded)
	assert.True(t, conn.Exceeded())
	assert.Equal(t, int32(1), bs.hits.Load(), "the refused request is never sent")
}

// TestGetBodyRefusesTheZeroPath asserts the zero Path is not requestable.
func TestGetBodyRefusesTheZeroPath(t *testing.T) {
	t.Parallel()
	bs := newBodyServer(t, http.StatusOK, `{}`)
	conn := NewConnection(bs.srv.Client(), "tok", bs.srv.URL, false, 5)
	_, err := conn.GetBody(t.Context(), Path{})
	require.ErrorIs(t, err, ErrEmptyPath)
	assert.Zero(t, bs.hits.Load())
}

// TestConnectionProjectsWithoutAListerFailsClosed asserts a Connection with
// no lister returns ErrNoLister and sends nothing.
func TestConnectionProjectsWithoutAListerFailsClosed(t *testing.T) {
	t.Parallel()
	bs := newBodyServer(t, http.StatusOK, `{}`)
	conn := NewConnection(bs.srv.Client(), "tok", bs.srv.URL, false, 5)
	items, err := conn.Projects(t.Context(), "c-abc")
	require.ErrorIs(t, err, ErrNoLister)
	assert.Nil(t, items)
	assert.Zero(t, bs.hits.Load())
	assert.Zero(t, conn.Used())
}

// TestConnectionProjectsCallsTheListerWithTheConnectionAndCluster asserts
// the lister receives the same Connection and the cluster id, its answer
// is returned, and removing it with nil restores the closed failure.
func TestConnectionProjectsCallsTheListerWithTheConnectionAndCluster(t *testing.T) {
	t.Parallel()
	conn := NewConnection(http.DefaultClient, "tok", "http://unused.invalid", false, 5)
	var gotConn *Connection
	var gotCluster string
	want := []ProjectItem{{ID: "p-1", DisplayName: "web"}}
	conn.SetProjectLister(func(_ context.Context, c *Connection, clusterID string) ([]ProjectItem, error) {
		gotConn, gotCluster = c, clusterID
		return want, nil
	})
	items, err := conn.Projects(t.Context(), "c-abc")
	require.NoError(t, err)
	assert.Equal(t, want, items)
	assert.Same(t, conn, gotConn)
	assert.Equal(t, "c-abc", gotCluster)

	conn.SetProjectLister(nil)
	_, err = conn.Projects(t.Context(), "c-abc")
	require.ErrorIs(t, err, ErrNoLister)
}

// TestConnectionProjectsReturnsTheListersError asserts a lister's error is
// returned as it is.
func TestConnectionProjectsReturnsTheListersError(t *testing.T) {
	t.Parallel()
	conn := NewConnection(http.DefaultClient, "tok", "http://unused.invalid", false, 5)
	boom := errors.New("boom")
	conn.SetProjectLister(func(context.Context, *Connection, string) ([]ProjectItem, error) { return nil, boom })
	_, err := conn.Projects(t.Context(), "c-abc")
	require.ErrorIs(t, err, boom)
}

// TestSteveNamespacedCollectionPath asserts the /v1/<resource>/<namespace>
// shape and that a segment ValidSegment refuses is refused with no path.
func TestSteveNamespacedCollectionPath(t *testing.T) {
	t.Parallel()
	p, err := SteveNamespacedCollectionPath("management.cattle.io.projects", "c-abc")
	require.NoError(t, err)
	assert.Equal(t, "/v1/management.cattle.io.projects/c-abc", p.String())

	for _, tc := range []struct{ resource, namespace string }{
		{"management.cattle.io.projects", ""},
		{"management.cattle.io.projects", ".."},
		{"management.cattle.io.projects", "a/b"},
		{"management.cattle.io.projects", "-c"},
		{"", "c-abc"},
		{"..", "c-abc"},
		{"a/b", "c-abc"},
	} {
		p, err := SteveNamespacedCollectionPath(tc.resource, tc.namespace)
		require.ErrorIs(t, err, ErrInvalidSegment, "%q %q", tc.resource, tc.namespace)
		assert.Empty(t, p.String())
	}
}

// TestConnectionClustersWithoutAListerFailsClosed asserts a Connection
// with no cluster lister returns ErrNoLister and sends nothing; an
// installed lister receives the Connection and its answer or error is
// returned as it is, and nil removes it.
func TestConnectionClustersWithoutAListerFailsClosed(t *testing.T) {
	t.Parallel()
	bs := newBodyServer(t, http.StatusOK, `{}`)
	conn := NewConnection(bs.srv.Client(), "tok", bs.srv.URL, false, 5)
	items, err := conn.Clusters(t.Context())
	require.ErrorIs(t, err, ErrNoLister)
	assert.Nil(t, items)
	assert.Zero(t, bs.hits.Load())
	assert.Zero(t, conn.Used())

	var gotConn *Connection
	want := []ClusterItem{{ID: "c-abc", DisplayName: "prod"}}
	conn.SetClusterLister(func(_ context.Context, c *Connection) ([]ClusterItem, error) {
		gotConn = c
		return want, nil
	})
	items, err = conn.Clusters(t.Context())
	require.NoError(t, err)
	assert.Equal(t, want, items)
	assert.Same(t, conn, gotConn)

	boom := errors.New("boom")
	conn.SetClusterLister(func(context.Context, *Connection) ([]ClusterItem, error) { return nil, boom })
	_, err = conn.Clusters(t.Context())
	require.ErrorIs(t, err, boom)

	conn.SetClusterLister(nil)
	_, err = conn.Clusters(t.Context())
	require.ErrorIs(t, err, ErrNoLister)
}

// TestSteveObjectPath asserts the /v1/<resource>/<name> shape and that a
// segment ValidSegment refuses is refused with no path.
func TestSteveObjectPath(t *testing.T) {
	t.Parallel()
	p, err := SteveObjectPath("management.cattle.io.clusters", "c-abc")
	require.NoError(t, err)
	assert.Equal(t, "/v1/management.cattle.io.clusters/c-abc", p.String())
	for _, tc := range []struct{ resource, name string }{
		{"management.cattle.io.clusters", ""},
		{"management.cattle.io.clusters", ".."},
		{"management.cattle.io.clusters", "a/b"},
		{"management.cattle.io.clusters", "prod cluster"},
		{"", "c-abc"},
	} {
		p, err := SteveObjectPath(tc.resource, tc.name)
		require.ErrorIs(t, err, ErrInvalidSegment, "%q %q", tc.resource, tc.name)
		assert.Empty(t, p.String())
	}
}
