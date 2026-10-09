package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
)

// steveClustersPath is the cluster collection the access commands read.
var steveClustersPath = steve.Request{Type: steve.ClustersType}.Path()

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

func steveItem(id, display string) map[string]any {
	return map[string]any{"id": id, "type": "management.cattle.io.cluster",
		"metadata": map[string]any{"name": id}, "spec": map[string]any{"displayName": display}}
}

// pagedServer serves the given pages in order, keyed by the continue token
// the previous page announced, and records every request path.
func pagedServer(t *testing.T, pages map[string]stevePage) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.URL.Path != steveClustersPath {
			http.NotFound(w, r)
			return
		}
		page, ok := pages[r.URL.Query().Get("continue")]
		if !ok {
			http.Error(w, "no such page", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	return srv, &paths
}

func TestListClustersReadsOnePageAndInsertsLocal(t *testing.T) {
	t.Parallel()
	srv, paths := pagedServer(t, map[string]stevePage{
		"": {Data: []map[string]any{steveItem("c-m-b", "staging"), steveItem("c-m-a", "prod")}, Count: new(2), Revision: "700"},
	})
	rows, err := listClusters(t.Context(), fakeRootClient{srv})
	require.NoError(t, err)
	assert.Equal(t, []discoveredCluster{
		{ID: "c-m-a", DisplayName: "prod", Returned: true},
		{ID: "c-m-b", DisplayName: "staging", Returned: true},
		{ID: "local", DisplayName: "local", Returned: false},
	}, rows)
	assert.Equal(t, []string{steveClustersPath}, *paths)
}

func TestListClustersKeepsAReturnedLocalRow(t *testing.T) {
	t.Parallel()
	srv, _ := pagedServer(t, map[string]stevePage{
		"": {Data: []map[string]any{steveItem("local", "local"), steveItem("c-m-a", "prod")}, Count: new(2), Revision: "1"},
	})
	rows, err := listClusters(t.Context(), fakeRootClient{srv})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.True(t, rows[1].Returned)
	assert.Equal(t, "local", rows[1].ID)
}

func TestListClustersEmptyCollectionIsLocalAlone(t *testing.T) {
	t.Parallel()
	srv, _ := pagedServer(t, map[string]stevePage{"": {Data: []map[string]any{}, Count: new(0), Revision: "5"}})
	rows, err := listClusters(t.Context(), fakeRootClient{srv})
	require.NoError(t, err)
	assert.Equal(t, []discoveredCluster{{ID: "local", DisplayName: "local"}}, rows)
}

func TestListClustersFollowsContinueAndEncodesTheToken(t *testing.T) {
	t.Parallel()
	srv, paths := pagedServer(t, map[string]stevePage{
		"":       {Data: []map[string]any{steveItem("c-m-a", "a")}, Count: new(3), Continue: "2 of 3", Revision: "9"},
		"2 of 3": {Data: []map[string]any{steveItem("c-m-b", "b")}, Count: new(3), Continue: "x&y", Revision: "9"},
		"x&y":    {Data: []map[string]any{steveItem("c-m-c", "c")}, Count: new(3), Revision: "9"},
	})
	rows, err := listClusters(t.Context(), fakeRootClient{srv})
	require.NoError(t, err)
	assert.Len(t, rows, 4)
	assert.Equal(t, []string{steveClustersPath, steveClustersPath + "?continue=2+of+3", steveClustersPath + "?continue=x%26y"}, *paths)
}

func TestListClustersRetriesOnceThenReportsAMovedRevision(t *testing.T) {
	t.Parallel()
	srv, paths := pagedServer(t, map[string]stevePage{
		"":  {Data: []map[string]any{steveItem("c-m-a", "a")}, Count: new(2), Continue: "n", Revision: "1"},
		"n": {Data: []map[string]any{steveItem("c-m-b", "b")}, Count: new(2), Revision: "2"},
	})
	_, err := listClusters(t.Context(), fakeRootClient{srv})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.True(t, ce.Retryable)
	require.ErrorContains(t, err, "revision moved between pages")
	assert.Len(t, *paths, 4, "the whole read is retried once before the failure is reported")
}

func TestListClustersRetryRecoversAConsistentSecondRead(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		page := stevePage{Data: []map[string]any{steveItem("c-m-a", "a")}, Count: new(1), Revision: "1"}
		if n == 1 {
			page.Count = new(2)
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	rows, err := listClusters(t.Context(), fakeRootClient{srv})
	require.NoError(t, err)
	assert.Len(t, rows, 2)
	assert.Equal(t, int32(2), calls.Load())
}

func TestListClustersProtocolErrorsAreTerminal(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pages map[string]stevePage
		want  string
	}{
		"missing id":     {map[string]stevePage{"": {Data: []map[string]any{{"metadata": map[string]any{"name": "x"}}}, Count: new(1), Revision: "1"}}, "has no id"},
		"name disagrees": {map[string]stevePage{"": {Data: []map[string]any{{"id": "c-1", "metadata": map[string]any{"name": "c-2"}}}, Count: new(1), Revision: "1"}}, `names itself "c-2"`},
		"no revision":    {map[string]stevePage{"": {Data: []map[string]any{steveItem("c-1", "")}, Count: new(1)}}, "without a revision"},
		"repeated token": {map[string]stevePage{"": {Data: []map[string]any{steveItem("c-1", "")}, Count: new(3), Continue: "t", Revision: "1"}, "t": {Data: []map[string]any{steveItem("c-2", "")}, Count: new(3), Continue: "t", Revision: "1"}}, "continue token repeated"},
		"missing count":  {map[string]stevePage{"": {Data: []map[string]any{steveItem("c-1", "")}, Revision: "1"}}, "without a count"},
		"negative count": {map[string]stevePage{"": {Data: []map[string]any{steveItem("c-1", "")}, Count: new(-1), Revision: "1"}}, "without a count"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, _ := pagedServer(t, tc.pages)
			_, err := listClusters(t.Context(), fakeRootClient{srv})
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.False(t, ce.Retryable)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestListClustersDuplicateMustAgree(t *testing.T) {
	t.Parallel()
	srv, _ := pagedServer(t, map[string]stevePage{
		"": {Data: []map[string]any{steveItem("c-1", "one"), steveItem("c-1", "one")}, Count: new(2), Revision: "1"},
	})
	rows, err := listClusters(t.Context(), fakeRootClient{srv})
	require.NoError(t, err)
	assert.Len(t, rows, 2, "an agreeing repeat is one row beside local")

	srv2, _ := pagedServer(t, map[string]stevePage{
		"": {Data: []map[string]any{steveItem("c-1", "one"), steveItem("c-1", "uno")}, Count: new(2), Revision: "1"},
	})
	_, err = listClusters(t.Context(), fakeRootClient{srv2})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.True(t, ce.Retryable)
	require.ErrorContains(t, err, "disagrees with itself")
}

func TestListClustersNotJSONIsTerminal(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	t.Cleanup(srv.Close)
	_, err := listClusters(t.Context(), fakeRootClient{srv})
	require.ErrorContains(t, err, "not a steve collection")
}

// TestListClustersRefusalKeepsTheCredentialError asserts a 401 on the
// cluster collection keeps the root client's AUTH error, and a 403, which
// Rancher never answers a signed-in user's collection read with, is RBAC
// naming the refused request.
func TestListClustersRefusalKeepsTheCredentialError(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]string{http.StatusUnauthorized: clierr.CodeAuth, http.StatusForbidden: clierr.CodeRBAC} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		t.Cleanup(srv.Close)
		_, err := listClusters(t.Context(), fakeRootClient{srv})
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, want, ce.Code, "status %d", status)
		if status == http.StatusForbidden {
			require.ErrorContains(t, err, "the server refused GET /v1/management.cattle.io.clusters (403)")
		}
	}
}

func TestListClustersCountMustAgreeAcrossPages(t *testing.T) {
	t.Parallel()
	srv, paths := pagedServer(t, map[string]stevePage{
		"":  {Data: []map[string]any{steveItem("c-1", "")}, Count: new(2), Continue: "n", Revision: "1"},
		"n": {Data: []map[string]any{steveItem("c-2", "")}, Count: new(3), Revision: "1"},
	})
	_, err := listClusters(t.Context(), fakeRootClient{srv})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.True(t, ce.Retryable)
	require.ErrorContains(t, err, "count moved between pages (2, then 3)")
	assert.Len(t, *paths, 4, "one whole-read retry before the failure")
}

func TestListClustersPageBoundIsTerminal(t *testing.T) {
	t.Parallel()
	// Every page announces a fresh token, so the read never ends on its own.
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := n.Add(1)
		_ = json.NewEncoder(w).Encode(stevePage{Data: []map[string]any{steveItem(fmt.Sprintf("c-%d", i), "")}, Count: new(1000), Continue: fmt.Sprintf("t%d", i), Revision: "1"})
	}))
	t.Cleanup(srv.Close)
	_, err := listClusters(t.Context(), fakeRootClient{srv})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.False(t, ce.Retryable)
	require.ErrorContains(t, err, "more than 100 pages")
	assert.Equal(t, int32(steve.MaxPages), n.Load())
}
