package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	accesscomplete "github.com/rancher/rancher-cli/internal/engine/access/complete"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clusterPagesServer serves pages of the steve clusters collection keyed by
// the continue token the previous page announced, and records every
// request URI.
func clusterPagesServer(t *testing.T, pages map[string]steveFixturePage) (*httptest.Server, func() []string, func()) {
	t.Helper()
	var mu sync.Mutex
	var uris []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uris = append(uris, r.URL.RequestURI())
		mu.Unlock()
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
	return srv, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(uris)
		}, func() {
			mu.Lock()
			defer mu.Unlock()
			uris = nil
		}
}

// factoryFor is the command's client factory against srvURL with a
// management token.
func factoryFor(t *testing.T, srvURL string) *clientFactory {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{CurrentServer: "srv", Servers: map[string]config.Server{"srv": {URL: srvURL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return newClientFactory(dir, &countingStore{token: "tok"})
}

func completeClusters(t *testing.T, src completion.Source, srv *httptest.Server, req completion.Request) ([]string, int) {
	t.Helper()
	conn := tracedConnection(srv.Client(), TokenCapability{Token: "tok", ServerURL: srv.URL}, steve.MaxPages)
	got := src.Complete(t.Context(), conn, req)
	return got.Names, conn.Used()
}

// TestClusterCompletionReadsWhatTheCommandReads asserts both cluster
// sources, through the surface's connection and cluster lister, and the
// command's own read of the collection, fed the same pages, agree: the
// same ids and display names, the same requests, `access projects` adding
// only the management cluster's row; and every value offered resolves
// through the command's factory to the cluster it names.
func TestClusterCompletionReadsWhatTheCommandReads(t *testing.T) {
	t.Parallel()
	pages := map[string]steveFixturePage{
		"":       {Data: []map[string]any{steveFixtureItem("c-m-b", "b")}, Count: new(4), Continue: "2 of 3", Revision: "9"},
		"2 of 3": {Data: []map[string]any{steveFixtureItem("c-m-a", "a"), steveFixtureItem("c-m-d", "dup")}, Count: new(4), Continue: "x&y", Revision: "9"},
		"x&y":    {Data: []map[string]any{steveFixtureItem("c-m-c", "dup")}, Count: new(4), Revision: "9"},
	}
	srv, requested, reset := clusterPagesServer(t, pages)

	f := factoryFor(t, srv.URL)
	get, list, err := f.clusters(t.Context())
	require.NoError(t, err)
	require.NotNil(t, get)
	commandRequests := requested()
	reset()

	shared, used := completeClusters(t, accesscomplete.SteveClusterSource(), srv, completion.Request{Flag: "cluster"})
	assert.Equal(t, commandRequests, requested(), "the same pages, in the same order")
	assert.Equal(t, len(commandRequests), used)
	reset()

	var want []string
	for _, c := range list {
		want = append(want, c.ID)
		if c.DisplayName != "dup" {
			want = append(want, c.DisplayName)
		}
	}
	assert.Equal(t, want, shared, "ids plus the display names that are unique")

	projects, _ := completeClusters(t, accesscomplete.ProjectsClusterSource(), srv, completion.Request{Flag: "cluster"})
	assert.Equal(t, commandRequests, requested())
	assert.ElementsMatch(t, append(slices.Clone(shared), "local"), projects)

	for _, v := range shared {
		id, err := f.ResolveCluster(t.Context(), v)
		require.NoError(t, err, "offered value %q resolves", v)
		assert.True(t, slices.ContainsFunc(list, func(c steve.Cluster) bool { return c.ID == id && (c.ID == v || c.DisplayName == v) }), "%q resolves to the cluster it names, got %q", v, id)
	}
}

// TestClusterCompletionRefusesWhatTheCommandRefuses asserts a read the
// command fails, a moved revision among them, empties both sources after
// one walk; the command retries the whole read once, completion never
// does: a moved revision or count costs completion the two pages of one
// walk.
func TestClusterCompletionRefusesWhatTheCommandRefuses(t *testing.T) {
	t.Parallel()
	item := steveFixtureItem
	tests := []struct {
		name  string
		pages map[string]steveFixturePage
		walk  int // the requests of one walk, when the row asserts them
	}{
		{name: "revision moved", walk: 2, pages: map[string]steveFixturePage{
			"":  {Data: []map[string]any{item("c-m-a", "a")}, Count: new(2), Continue: "n", Revision: "1"},
			"n": {Data: []map[string]any{item("c-m-b", "b")}, Count: new(2), Revision: "2"}}},
		{name: "count moved", walk: 2, pages: map[string]steveFixturePage{
			"":  {Data: []map[string]any{item("c-m-a", "a")}, Count: new(2), Continue: "n", Revision: "1"},
			"n": {Data: []map[string]any{item("c-m-b", "b")}, Count: new(3), Revision: "1"}}},
		{name: "no revision", pages: map[string]steveFixturePage{
			"": {Data: []map[string]any{item("c-m-a", "a")}, Count: new(1)}}},
		{name: "no count", pages: map[string]steveFixturePage{
			"": {Data: []map[string]any{item("c-m-a", "a")}, Revision: "1"}}},
		{name: "item without id", pages: map[string]steveFixturePage{
			"": {Data: []map[string]any{{"metadata": map[string]any{"name": "x"}}}, Count: new(1), Revision: "1"}}},
		{name: "item names itself otherwise", pages: map[string]steveFixturePage{
			"": {Data: []map[string]any{{"id": "c-m-a", "metadata": map[string]any{"name": "c-m-b"}}}, Count: new(1), Revision: "1"}}},
		{name: "repeated item disagrees", pages: map[string]steveFixturePage{
			"": {Data: []map[string]any{item("c-m-a", "a"), item("c-m-a", "b")}, Count: new(2), Revision: "1"}}},
		{name: "continue token repeats", pages: map[string]steveFixturePage{
			"":  {Data: []map[string]any{item("c-m-a", "a")}, Count: new(2), Continue: "n", Revision: "1"},
			"n": {Data: []map[string]any{item("c-m-b", "b")}, Count: new(2), Continue: "n", Revision: "1"}}},
		{name: "count does not match what was read", pages: map[string]steveFixturePage{
			"": {Data: []map[string]any{item("c-m-a", "a")}, Count: new(2), Revision: "1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, requested, reset := clusterPagesServer(t, tt.pages)
			_, _, err := factoryFor(t, srv.URL).clusters(t.Context())
			require.Error(t, err, "the command refuses the read")
			commandRequests := len(requested())
			reset()
			for _, src := range []completion.Source{accesscomplete.SteveClusterSource(), accesscomplete.ProjectsClusterSource()} {
				names, used := completeClusters(t, src, srv, completion.Request{Flag: "cluster"})
				assert.Empty(t, names)
				assert.LessOrEqual(t, used, commandRequests, "one walk at most")
				if tt.walk > 0 {
					assert.Equal(t, tt.walk, used, "one walk, never retried")
				}
			}
		})
	}

	t.Run("a page past the allowance", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := clusterPagesServer(t, map[string]steveFixturePage{
			"":  {Data: []map[string]any{item("c-m-a", "a")}, Count: new(2), Continue: "n", Revision: "1"},
			"n": {Data: []map[string]any{item("c-m-b", "b")}, Count: new(2), Revision: "1"},
		})
		conn := tracedConnection(srv.Client(), TokenCapability{Token: "tok", ServerURL: srv.URL}, 1)
		assert.Empty(t, accesscomplete.SteveClusterSource().Complete(t.Context(), conn, completion.Request{Flag: "cluster"}).Names)
	})
}
