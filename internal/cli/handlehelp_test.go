package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHelpNamesTheIDOnKindsWithAHandle asserts the usage line of get shows the
// optional id and of patch and delete the required id for every kind whose
// argument is an id, and that the management cluster and a kind addressed by
// name keep name.
func TestHelpNamesTheIDOnKindsWithAHandle(t *testing.T) {
	t.Parallel()
	usage := func(t *testing.T, path ...string) string {
		t.Helper()
		out := renderHelp(t, "", path...)
		m := regexp.MustCompile(`(?m)^USAGE:\n\s+(rancher .*)$`).FindStringSubmatch(out)
		require.NotNil(t, m, strings.Join(path, " "))
		return m[1]
	}
	count := 0
	for _, k := range kinds.All() {
		if k.Handle == nil {
			continue
		}
		count++
		assert.Equal(t, "rancher get "+k.Name+" [options] [id]", usage(t, "get", k.Name), k.Name)
		assert.Equal(t, "rancher patch "+k.Name+" [options] <id>", usage(t, "patch", k.Name), k.Name)
		if k.Deletable {
			assert.Equal(t, "rancher delete "+k.Name+" [options] <id>", usage(t, "delete", k.Name), k.Name)
		}
	}
	assert.Equal(t, 5, count)
	assert.Equal(t, "rancher get cluster [options] [name]", usage(t, "get", "cluster"))
	assert.Equal(t, "rancher patch cluster [options] <name>", usage(t, "patch", "cluster"))
	assert.Equal(t, "rancher delete clusterroletemplatebinding [options] <name>", usage(t, "delete", "clusterroletemplatebinding"))
}

var _ engine.ProjectNameLister = (*clientFactory)(nil)

// projectsRoot serves the steve projects collection of one cluster in one
// page and records every path.
type projectsRoot struct {
	mu    sync.Mutex
	paths []string
	items []map[string]any
}

func projectItem(cluster, id, display string) map[string]any {
	return map[string]any{"id": cluster + "/" + id, "metadata": map[string]any{"name": id},
		"spec": map[string]any{"clusterName": cluster, "displayName": display}}
}

func (r *projectsRoot) GetJSON(_ context.Context, path string) ([]byte, error) {
	r.mu.Lock()
	r.paths = append(r.paths, path)
	r.mu.Unlock()
	if path != "/v1/management.cattle.io.projects/c-abc" {
		return nil, clierr.New(clierr.CodeGeneric, "unexpected GET "+path).
			WithCause(clierr.ResponseError(http.MethodGet, http.StatusNotFound, nil))
	}
	return json.Marshal(map[string]any{"data": r.items, "count": len(r.items), "revision": "1"})
}

// TestListProjectsReadsTheTypedClustersProjectsThroughSteve asserts the
// factory lists the caller's projects of one cluster with the root client, as
// ids and display names, in one request.
func TestListProjectsReadsTheTypedClustersProjectsThroughSteve(t *testing.T) {
	t.Parallel()
	root := &projectsRoot{items: []map[string]any{projectItem("c-abc", "p-1", "Web Team"), projectItem("c-abc", "p-2", "Data")}}
	f := newClientFactory(writeTestConfig(t), &countingStore{token: "id:secret"})
	f.rootOnce.Do(func() { f.root = root })

	got, err := f.ListProjects(t.Context(), "c-abc")
	require.NoError(t, err)
	assert.Equal(t, []engine.ProjectRef{{ID: "p-1", DisplayName: "Web Team"}, {ID: "p-2", DisplayName: "Data"}}, got)
	assert.Equal(t, []string{"/v1/management.cattle.io.projects/c-abc"}, root.paths)
}

// TestListProjectsReturnsTheReadError asserts a failed read is returned, for
// the caller to leave the original miss as it was.
func TestListProjectsReturnsTheReadError(t *testing.T) {
	t.Parallel()
	f := newClientFactory(writeTestConfig(t), &countingStore{token: "id:secret"})
	f.rootOnce.Do(func() { f.root = &projectsRoot{} })
	_, err := f.ListProjects(t.Context(), "c-other")
	require.ErrorContains(t, err, "answered 404; this server does not serve Rancher's steve API as expected")
}

// TestGetProjectByDisplayNameNamesTheIDThroughTheFactory asserts a named get
// of a project by its display name, run through the real root and client
// factory, lists the cluster's projects through steve and names the id, the
// candidate and the hint with the cluster. Not parallel: the harness sets the
// process environment.
func TestGetProjectByDisplayNameNamesTheIDThroughTheFactory(t *testing.T) {
	h := newCallerHarness(t, false)
	h.srv.projectMiss.Store(true)
	r := h.run(t, strings.Fields("get project web --cluster prod -o json"))
	require.NotNil(t, r.err, r.stderr)
	assert.Equal(t, "NOT_FOUND", r.err.Code)
	assert.Equal(t, "project not found: web; 'web' is the display name of project p-1", r.err.Message)
	assert.Equal(t, "rancher get project 'p-1' --cluster 'c-abc'", r.err.Hint)

	var env struct {
		Data struct {
			Error struct {
				Candidates []clierr.Candidate `json:"candidates"`
			} `json:"error"`
		} `json:"data"`
	}
	start := strings.Index(r.stderr, "{")
	require.GreaterOrEqual(t, start, 0, "the error envelope is on stderr: %q", r.stderr)
	require.NoError(t, json.Unmarshal([]byte(r.stderr[start:]), &env))
	assert.Equal(t, []clierr.Candidate{{ID: "p-1", Name: "web"}}, env.Data.Error.Candidates)
	assert.True(t, slices.ContainsFunc(r.requests, func(q callerRequest) bool {
		return q.method == http.MethodGet && q.path == testrancher.ProjectsCollectionPath+"/c-abc"
	}), "the projects of the typed cluster are read through steve: %v", r.requests)
}
