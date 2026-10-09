package steve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

var clustersPath = Request{Type: ClustersType}.Path()

// fakeRootClient answers engine.RootClient over a real httptest server with
// the production root client's non-2xx answer (clierr.FromResponse with the
// same prefix and request), so the reader's decode and consistency rules, not
// a substitute for them, are what the tests exercise.
type fakeRootClient struct{ srv *httptest.Server }

func (f fakeRootClient) GetJSON(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		return nil, clierr.New(clierr.CodeGeneric, "rancher root GET "+path+": "+err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	return nil, clierr.FromResponse("rancher root GET "+path, http.MethodGet, resp.StatusCode, body, &clierr.Request{})
}

var _ engine.RootClient = fakeRootClient{}

type stevePage struct {
	Data     []map[string]any `json:"data"`
	Count    *int             `json:"count,omitempty"`
	Continue string           `json:"continue,omitempty"`
	Revision string           `json:"revision"`
}

// route is what a test server answers for one URL path: pages indexed by the
// continue token the previous page announced, or one status for every
// request.
type route struct {
	pages  map[string]stevePage
	status int
}

type recordingServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []string
}

// newServer serves routes by URL path and records every request URI; a path
// with no route answers 404.
func newServer(t *testing.T, routes map[string]route) *recordingServer {
	t.Helper()
	return newHandlerServer(t, func(w http.ResponseWriter, r *http.Request) {
		rt, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rt.status != 0 {
			w.WriteHeader(rt.status)
			return
		}
		page, ok := rt.pages[r.URL.Query().Get("continue")]
		if !ok {
			http.Error(w, "no such page", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	})
}

func newHandlerServer(t *testing.T, h http.HandlerFunc) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.got = append(rs.got, r.URL.RequestURI())
		rs.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

func (rs *recordingServer) requests() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return slices.Clone(rs.got)
}

func (rs *recordingServer) getter() Getter { return RootGetter(fakeRootClient{rs.srv}) }

func steveCluster(id, display string) map[string]any {
	return map[string]any{"id": id, "type": "management.cattle.io.cluster",
		"metadata": map[string]any{"name": id}, "spec": map[string]any{"displayName": display}}
}

func clustersServer(t *testing.T, pages map[string]stevePage) *recordingServer {
	t.Helper()
	return newServer(t, map[string]route{clustersPath: {pages: pages}})
}

func retryable(t *testing.T, err error) bool {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	return ce.Retryable
}

// TestRequestPath asserts the path of every request shape: the root and
// per-cluster prefixes, escaped namespace and name segments, and the
// escaped continue token.
func TestRequestPath(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		r    Request
		want string
	}{
		"root collection":    {Request{Type: "management.cattle.io.clusters"}, "/v1/management.cattle.io.clusters"},
		"cluster collection": {Request{Cluster: "c-abc", Type: "namespaces"}, "/k8s/clusters/c-abc/v1/namespaces"},
		"namespace":          {Request{Type: ProjectsType, Namespace: "c-abc"}, "/v1/management.cattle.io.projects/c-abc"},
		"namespace and name": {Request{Cluster: "c-abc", Type: "secrets", Namespace: "ns", Name: "s"}, "/k8s/clusters/c-abc/v1/secrets/ns/s"},
		"escaped segments":   {Request{Cluster: "c/x", Type: "t y", Namespace: "a b", Name: "n/m"}, "/k8s/clusters/c%2Fx/v1/t%20y/a%20b/n%2Fm"},
		"continue":           {Request{Type: "namespaces", Continue: "2 of 3"}, "/v1/namespaces?continue=2+of+3"},
		"continue escaped":   {Request{Type: "namespaces", Namespace: "x", Continue: "a&b=c/d"}, "/v1/namespaces/x?continue=a%26b%3Dc%2Fd"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.r.Path())
		})
	}
}

// TestRootGetterKeepsTheRootErrorAndAddsTheStatus asserts a non-2xx answer
// stays the root client's *clierr.Error with a status error cause (the one
// clierr.ResponseError builds) that clierr.HTTPStatus reads, while an error
// with no status gains none and a 2xx returns the body.
func TestRootGetterKeepsTheRootErrorAndAddsTheStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			srv := newServer(t, map[string]route{"/v1/x": {status: status}})
			_, err := srv.getter()(t.Context(), Request{Type: "x"})
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			wantCode := map[int]string{401: clierr.CodeAuth, 403: clierr.CodeRBAC, 404: clierr.CodeNotFound, 500: clierr.CodeGeneric}[status]
			assert.Equal(t, wantCode, ce.Code)
			got, ok := clierr.HTTPStatus(err)
			require.True(t, ok)
			assert.Equal(t, status, got)
			require.ErrorContains(t, err, "rancher root GET /v1/x")
		})
	}

	t.Run("body", func(t *testing.T) {
		t.Parallel()
		srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"a":1}`)) })
		body, err := srv.getter()(t.Context(), Request{Type: "x"})
		require.NoError(t, err)
		assert.JSONEq(t, `{"a":1}`, string(body))
	})

	t.Run("no status", func(t *testing.T) {
		t.Parallel()
		srv := newHandlerServer(t, func(http.ResponseWriter, *http.Request) {})
		srv.srv.Close()
		_, err := srv.getter()(t.Context(), Request{Type: "x"})
		require.Error(t, err)
		_, ok := clierr.HTTPStatus(err)
		assert.False(t, ok, "a transport failure has no status")
	})
}

// TestReadCollectionOnePage asserts one page with a count reads in one
// request, and a zero count with empty data is zero rows.
func TestReadCollectionOnePage(t *testing.T) {
	t.Parallel()
	srv := clustersServer(t, map[string]stevePage{
		"": {Data: []map[string]any{steveCluster("c-b", "B"), steveCluster("c-a", "A")}, Count: new(2), Revision: "7"},
	})
	rows, err := Clusters(t.Context(), srv.getter())
	require.NoError(t, err)
	assert.Equal(t, []Cluster{{ID: "c-b", DisplayName: "B"}, {ID: "c-a", DisplayName: "A"}}, rows, "collection order is kept")
	assert.Equal(t, []string{clustersPath}, srv.requests())

	zero := clustersServer(t, map[string]stevePage{"": {Data: []map[string]any{}, Count: new(0), Revision: "5"}})
	rows, err = Clusters(t.Context(), zero.getter())
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestReadCollectionFollowsContinueAndEncodesTheToken asserts every page
// is read with the previous page's token URL-encoded.
func TestReadCollectionFollowsContinueAndEncodesTheToken(t *testing.T) {
	t.Parallel()
	srv := clustersServer(t, map[string]stevePage{
		"":       {Data: []map[string]any{steveCluster("c-a", "a")}, Count: new(3), Continue: "2 of 3", Revision: "9"},
		"2 of 3": {Data: []map[string]any{steveCluster("c-b", "b")}, Count: new(3), Continue: "x&y", Revision: "9"},
		"x&y":    {Data: []map[string]any{steveCluster("c-c", "c")}, Count: new(3), Revision: "9"},
	})
	rows, err := Clusters(t.Context(), srv.getter())
	require.NoError(t, err)
	assert.Len(t, rows, 3)
	assert.Equal(t, []string{clustersPath, clustersPath + "?continue=2+of+3", clustersPath + "?continue=x%26y"}, srv.requests())
}

// TestReadCollectionEmptyFirstPageWithoutCountIsZeroRows asserts the
// apiserver's omitted zero count: an empty first and only page with no
// count and no continue is an empty collection, and the same page with a
// continue token is not.
func TestReadCollectionEmptyFirstPageWithoutCountIsZeroRows(t *testing.T) {
	t.Parallel()
	srv := clustersServer(t, map[string]stevePage{"": {Data: []map[string]any{}, Revision: "5"}})
	rows, err := Clusters(t.Context(), srv.getter())
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.Len(t, srv.requests(), 1)

	more := clustersServer(t, map[string]stevePage{"": {Data: []map[string]any{}, Continue: "n", Revision: "5"}})
	_, err = Clusters(t.Context(), more.getter())
	require.ErrorContains(t, err, "without a count")
	assert.False(t, retryable(t, err))
}

// TestReadCollectionProtocolErrorsAreTerminal asserts a response the reader
// cannot trust fails on the first read, never retried, as a generic error
// naming the problem.
func TestReadCollectionProtocolErrorsAreTerminal(t *testing.T) {
	t.Parallel()
	item := []map[string]any{steveCluster("c-1", "")}
	for name, tc := range map[string]struct {
		pages map[string]stevePage
		want  string
	}{
		"data without count":       {map[string]stevePage{"": {Data: item, Revision: "1"}}, "without a count"},
		"negative count":           {map[string]stevePage{"": {Data: item, Count: new(-1), Revision: "1"}}, "without a count"},
		"no count on a later page": {map[string]stevePage{"": {Data: item, Count: new(2), Continue: "n", Revision: "1"}, "n": {Data: []map[string]any{steveCluster("c-2", "")}, Revision: "1"}}, "without a count"},
		"no revision":              {map[string]stevePage{"": {Data: item, Count: new(1)}}, "without a revision"},
		"repeated continue token":  {map[string]stevePage{"": {Data: item, Count: new(3), Continue: "t", Revision: "1"}, "t": {Data: []map[string]any{steveCluster("c-2", "")}, Count: new(3), Continue: "t", Revision: "1"}}, "the continue token repeated"},
		"item without an id":       {map[string]stevePage{"": {Data: []map[string]any{{"metadata": map[string]any{"name": "x"}}}, Count: new(1), Revision: "1"}}, "item 0 has no id"},
		"metadata name disagrees":  {map[string]stevePage{"": {Data: []map[string]any{{"id": "c-1", "metadata": map[string]any{"name": "c-2"}}}, Count: new(1), Revision: "1"}}, `"c-1" names itself "c-2"`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := clustersServer(t, tc.pages)
			_, err := Clusters(t.Context(), srv.getter())
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.False(t, ce.Retryable)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			require.ErrorContains(t, err, tc.want)
			assert.LessOrEqual(t, len(srv.requests()), 2, "terminal on the first read, no second walk")
		})
	}
}

// TestReadCollectionNotJSONIsTerminal asserts a body that is not a JSON
// collection fails on the first read.
func TestReadCollectionNotJSONIsTerminal(t *testing.T) {
	t.Parallel()
	srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) })
	_, err := Clusters(t.Context(), srv.getter())
	require.ErrorContains(t, err, "not a steve collection")
	assert.False(t, retryable(t, err))
	assert.Len(t, srv.requests(), 1)
}

// TestReadCollectionCountMovedBetweenPagesIsRetryable asserts a count that
// differs between pages is retried once as a whole, then reported as a
// retryable change.
func TestReadCollectionCountMovedBetweenPagesIsRetryable(t *testing.T) {
	t.Parallel()
	srv := clustersServer(t, map[string]stevePage{
		"":  {Data: []map[string]any{steveCluster("c-1", "")}, Count: new(2), Continue: "n", Revision: "1"},
		"n": {Data: []map[string]any{steveCluster("c-2", "")}, Count: new(3), Revision: "1"},
	})
	_, err := Clusters(t.Context(), srv.getter())
	assert.True(t, retryable(t, err))
	require.ErrorContains(t, err, "count moved between pages (2, then 3)")
	assert.Len(t, srv.requests(), 4, "two walks of two pages")
}

// TestReadCollectionRevisionMovedIsRetriedOnceThenRetryable asserts a
// revision that moves between pages makes exactly two whole walks.
func TestReadCollectionRevisionMovedIsRetriedOnceThenRetryable(t *testing.T) {
	t.Parallel()
	srv := clustersServer(t, map[string]stevePage{
		"":  {Data: []map[string]any{steveCluster("c-a", "a")}, Count: new(2), Continue: "n", Revision: "1"},
		"n": {Data: []map[string]any{steveCluster("c-b", "b")}, Count: new(2), Revision: "2"},
	})
	_, err := Clusters(t.Context(), srv.getter())
	assert.True(t, retryable(t, err))
	require.ErrorContains(t, err, "revision moved between pages")
	assert.Equal(t, []string{clustersPath, clustersPath + "?continue=n", clustersPath, clustersPath + "?continue=n"}, srv.requests())
}

// TestReadCollectionRetryRecoversAConsistentSecondRead asserts a second
// walk that is consistent returns its rows.
func TestReadCollectionRetryRecoversAConsistentSecondRead(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) {
		page := stevePage{Data: []map[string]any{steveCluster("c-a", "a")}, Count: new(1), Revision: "1"}
		if calls.Add(1) == 1 {
			page.Count = new(2)
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	rows, err := Clusters(t.Context(), srv.getter())
	require.NoError(t, err)
	assert.Equal(t, []Cluster{{ID: "c-a", DisplayName: "a"}}, rows)
	assert.Equal(t, int32(2), calls.Load())
}

// TestReadCollectionCountMustMatchTheRowsRead asserts a total that differs
// from the items read is a retryable change after two walks.
func TestReadCollectionCountMustMatchTheRowsRead(t *testing.T) {
	t.Parallel()
	srv := clustersServer(t, map[string]stevePage{
		"": {Data: []map[string]any{steveCluster("c-1", "")}, Count: new(2), Revision: "1"},
	})
	_, err := Clusters(t.Context(), srv.getter())
	assert.True(t, retryable(t, err))
	require.ErrorContains(t, err, "count 2, read 1")
	assert.Len(t, srv.requests(), 2)
}

// TestReadCollectionRepeatedContinueTokenIsTerminal asserts a token the
// server hands out twice ends the read at once without a second walk.
func TestReadCollectionRepeatedContinueTokenIsTerminal(t *testing.T) {
	t.Parallel()
	srv := clustersServer(t, map[string]stevePage{
		"":  {Data: []map[string]any{steveCluster("c-1", "")}, Count: new(3), Continue: "t", Revision: "1"},
		"t": {Data: []map[string]any{steveCluster("c-2", "")}, Count: new(3), Continue: "t", Revision: "1"},
	})
	_, err := Clusters(t.Context(), srv.getter())
	assert.False(t, retryable(t, err))
	require.ErrorContains(t, err, "the continue token repeated")
	assert.Len(t, srv.requests(), 2)
}

// TestReadCollectionPageBoundIsTerminal asserts a server that keeps handing
// out fresh tokens is read for exactly MaxPages requests, then fails.
func TestReadCollectionPageBoundIsTerminal(t *testing.T) {
	t.Parallel()
	var n atomic.Int32
	srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) {
		i := n.Add(1)
		_ = json.NewEncoder(w).Encode(stevePage{Data: []map[string]any{steveCluster(fmt.Sprintf("c-%d", i), "")}, Count: new(1000), Continue: fmt.Sprintf("t%d", i), Revision: "1"})
	})
	_, err := Clusters(t.Context(), srv.getter())
	assert.False(t, retryable(t, err))
	require.ErrorContains(t, err, fmt.Sprintf("more than %d pages", MaxPages))
	assert.Equal(t, int32(MaxPages), n.Load())
}

// TestReadCollectionDuplicates asserts an item repeated with the same
// content is one row and one that disagrees with itself is a retryable
// changed collection after two walks.
func TestReadCollectionDuplicates(t *testing.T) {
	t.Parallel()
	agree := clustersServer(t, map[string]stevePage{
		"": {Data: []map[string]any{steveCluster("c-1", "one"), steveCluster("c-1", "one")}, Count: new(2), Revision: "1"},
	})
	rows, err := Clusters(t.Context(), agree.getter())
	require.NoError(t, err)
	assert.Equal(t, []Cluster{{ID: "c-1", DisplayName: "one"}}, rows)

	disagree := clustersServer(t, map[string]stevePage{
		"": {Data: []map[string]any{steveCluster("c-1", "one"), steveCluster("c-1", "uno")}, Count: new(2), Revision: "1"},
	})
	_, err = Clusters(t.Context(), disagree.getter())
	assert.True(t, retryable(t, err))
	require.ErrorContains(t, err, "repeated item c-1 disagrees with itself")
	assert.Len(t, disagree.requests(), 2)
}

// TestClustersOnceWalksOnceAndNeverRetries asserts the single-walk read
// follows every page, and a collection that moves under it is reported
// after exactly one walk, where Clusters walks twice.
func TestClustersOnceWalksOnceAndNeverRetries(t *testing.T) {
	t.Parallel()
	ok := clustersServer(t, map[string]stevePage{
		"":  {Data: []map[string]any{steveCluster("c-b", "staging")}, Count: new(2), Continue: "n", Revision: "700"},
		"n": {Data: []map[string]any{steveCluster("c-a", "prod")}, Count: new(2), Revision: "700"},
	})
	rows, err := ClustersOnce(t.Context(), ok.getter())
	require.NoError(t, err)
	assert.Equal(t, []Cluster{{ID: "c-b", DisplayName: "staging"}, {ID: "c-a", DisplayName: "prod"}}, rows)
	assert.Len(t, ok.requests(), 2)

	moved := map[string]stevePage{
		"":  {Data: []map[string]any{steveCluster("c-a", "a")}, Count: new(2), Continue: "n", Revision: "1"},
		"n": {Data: []map[string]any{steveCluster("c-b", "b")}, Count: new(2), Revision: "2"},
	}
	once := clustersServer(t, moved)
	_, err = ClustersOnce(t.Context(), once.getter())
	assert.True(t, retryable(t, err))
	require.ErrorContains(t, err, "revision moved between pages")
	assert.Equal(t, []string{clustersPath, clustersPath + "?continue=n"}, once.requests(), "one walk")

	twice := clustersServer(t, moved)
	_, err = Clusters(t.Context(), twice.getter())
	require.Error(t, err)
	assert.Len(t, twice.requests(), 4, "the retrying read walks twice")
}

type fakeRootFactory struct {
	rc    engine.RootClient
	err   error
	calls *atomic.Int32
}

func (f fakeRootFactory) Root() (engine.RootClient, error) {
	f.calls.Add(1)
	return f.rc, f.err
}

// TestRootFactoryGetterAsksForTheRootAtTheRequest asserts the getter asks
// the factory for the root client only when it sends a request, reads
// through it as RootGetter does, and returns the factory's error.
func TestRootFactoryGetterAsksForTheRootAtTheRequest(t *testing.T) {
	t.Parallel()
	srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"a":1}`)) })
	calls := &atomic.Int32{}
	get := RootFactoryGetter(fakeRootFactory{rc: fakeRootClient{srv.srv}, calls: calls})
	assert.Zero(t, calls.Load(), "building the getter asks for nothing")
	body, err := get(t.Context(), Request{Type: "x"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":1}`, string(body))
	assert.Equal(t, int32(1), calls.Load())

	boom := errors.New("no server configured")
	_, err = RootFactoryGetter(fakeRootFactory{err: boom, calls: calls})(t.Context(), Request{Type: "x"})
	require.ErrorIs(t, err, boom)
}

// TestReadCollectionReturnsARetryableRefusalAtOnce asserts a retryable
// server answer (here a 503) is returned after one read: only a
// collection whose pages disagree is read again.
func TestReadCollectionReturnsARetryableRefusalAtOnce(t *testing.T) {
	t.Parallel()
	var calls int
	get := func(context.Context, Request) ([]byte, error) {
		calls++
		return nil, clierr.FromResponse("GET /v1/x", http.MethodGet, http.StatusServiceUnavailable, nil, &clierr.Request{})
	}
	_, err := ReadCollection(t.Context(), get, Request{Type: ClustersType}, func(string, int, json.RawMessage) (string, string, error) {
		return "", "", nil
	})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.True(t, ce.Retryable)
	assert.Equal(t, 1, calls)
}
