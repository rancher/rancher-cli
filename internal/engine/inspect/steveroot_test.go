package inspect

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/require"
)

// steveAnswer is one path's answer: a status and, for a 2xx, a JSON body.
type steveAnswer struct {
	status int
	body   any
}

// steveRoot answers engine.RootClient over a real httptest server with the
// production root client's non-2xx answer (clierr.FromResponse with the
// same prefix and request), so steve's decode and status rules run on its
// answers. A path with no answer gets 404. Every request path is recorded.
type steveRoot struct {
	srv *httptest.Server

	mu      sync.Mutex
	answers map[string]steveAnswer
	paths   []string
}

func newSteveRoot(t *testing.T, answers map[string]steveAnswer) *steveRoot {
	t.Helper()
	r := &steveRoot{answers: map[string]steveAnswer{}}
	maps.Copy(r.answers, answers)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.RequestURI())
		a, ok := r.answers[req.URL.RequestURI()]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		if a.status < 200 || a.status > 299 {
			w.WriteHeader(a.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		_ = json.NewEncoder(w).Encode(a.body)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// answer sets path's answer; a test calls it before the command runs.
func (r *steveRoot) answer(path string, a steveAnswer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[path] = a
}

// requests returns every path the root was asked for, in order.
func (r *steveRoot) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.paths)
}

func (r *steveRoot) GetJSON(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.srv.URL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.srv.Client().Do(req)
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

var _ engine.RootClient = (*steveRoot)(nil)

// roots hands the root out as the command's engine.RootClientFactory.
func (r *steveRoot) roots() engine.RootClientFactory { return fakeRoots{rc: r} }

// steveProjectsPath is the path of clusterID's steve projects collection.
func steveProjectsPath(clusterID string) string {
	return steve.Request{Type: steve.ProjectsType, Namespace: clusterID}.Path()
}

// steveNamespacesPath is the path of clusterID's steve namespaces collection.
func steveNamespacesPath(clusterID string) string {
	return steve.Request{Cluster: clusterID, Type: "namespaces"}.Path()
}

// stevePage is a one-page steve collection answer holding items.
func stevePage(items ...map[string]any) steveAnswer {
	if items == nil {
		items = []map[string]any{}
	}
	return steveAnswer{status: http.StatusOK, body: map[string]any{"data": items, "count": len(items), "revision": "1"}}
}

// steveProjectItem is one item of a steve projects collection.
func steveProjectItem(clusterID, id, display string) map[string]any {
	return map[string]any{"id": clusterID + "/" + id, "type": "management.cattle.io.project",
		"metadata": map[string]any{"name": id},
		"spec":     map[string]any{"clusterName": clusterID, "displayName": display}}
}

// steveClusterItem is one item of the steve management clusters collection.
func steveClusterItem(id, display string) map[string]any {
	return map[string]any{"id": id, "type": "management.cattle.io.cluster",
		"metadata": map[string]any{"name": id},
		"spec":     map[string]any{"displayName": display}}
}

// clustersRoot is a root serving the steve management clusters collection
// with one cluster per id.
func clustersRoot(t *testing.T, ids ...string) *steveRoot {
	t.Helper()
	items := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		items = append(items, steveClusterItem(id, id+"-name"))
	}
	return newSteveRoot(t, map[string]steveAnswer{steveClusterPath(""): stevePage(items...)})
}

// projectsRoot is a root serving clusterID's projects collection with the
// given items.
func projectsRoot(t *testing.T, clusterID string, items ...map[string]any) *steveRoot {
	t.Helper()
	return newSteveRoot(t, map[string]steveAnswer{steveProjectsPath(clusterID): stevePage(items...)})
}

// withProject is t.Context() carrying the project the pipeline resolved,
// whose namespaces are members.
func withProject(t *testing.T, clusterID, id, display string, members ...string) context.Context {
	t.Helper()
	set := make(map[string]bool, len(members))
	for _, m := range members {
		set[m] = true
	}
	return runtime.WithProject(t.Context(), runtime.Project{Cluster: clusterID, ID: id, DisplayName: display,
		Namespaces: func(context.Context) (map[string]bool, error) { return set, nil }})
}

// execReadCtx is execRead with the caller's context.
func execReadCtx(t *testing.T, ctx context.Context, cmd runtime.Command, in runtime.Input) (runtime.Result, error) {
	t.Helper()
	rc, ok := cmd.(runtime.ReadCommand)
	require.True(t, ok, "command must be a ReadCommand")
	return rc.Execute(ctx, in, nil)
}
