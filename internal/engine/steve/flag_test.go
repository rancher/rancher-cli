package steve

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// fakeFactory is an engine.ClusterClientFactory that records the names it
// resolves and the clusters it hands a client for. Methods it does not
// override panic through the nil embedded interface.
type fakeFactory struct {
	engine.ClusterClientFactory
	mu         sync.Mutex
	resolved   []string
	clusters   []string
	resolveTo  string
	resolveErr error
	clusterErr error
	client     engine.ClusterClient
}

func (f *fakeFactory) ResolveCluster(_ context.Context, nameOrID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, nameOrID)
	return f.resolveTo, f.resolveErr
}

func (f *fakeFactory) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clusters = append(f.clusters, id)
	return f.client, f.clusterErr
}

func clustersAndProjects(t *testing.T, projects []map[string]any) *recordingServer {
	t.Helper()
	return newServer(t, map[string]route{
		allProjectsPath: {pages: onePage(projects...)},
		clustersPath: {pages: onePage(
			steveCluster("c-abc", "Alpha"), steveCluster("c-def", "Beta"), steveCluster("c-ghi", "Gamma"),
		)},
	})
}

func TestResolveProjectFlagWithoutClusterNamesTheOneClusterThatHasIt(t *testing.T) {
	t.Parallel()
	access := clierr.Rancher + " access clusters"
	for name, tc := range map[string]struct{ v, message, hint string }{
		"display name": {"db", "--cluster is required with --project; the project with the id or display name 'db' is on cluster 'c-abc' - pass --cluster 'c-abc'", access},
		// p-1 is one project's id and two others' display name: on the
		// cluster the id wins, so --cluster alone resolves it.
		"id that is also a display name": {"p-1", "--cluster is required with --project; the project with the id or display name 'p-1' is on cluster 'c-abc' - pass --cluster 'c-abc'", access},
		"display name of two projects": {"web",
			"--cluster is required with --project; 2 projects with the display name 'web' are on cluster 'c-abc' - pass --cluster 'c-abc' and one of their ids as --project",
			clierr.Rancher + " access projects --cluster 'c-abc'"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := clustersAndProjects(t, []map[string]any{
				steveProject("c-abc", "p-1", "web"), steveProject("c-abc", "p-8", "web"), steveProject("c-abc", "p-3", "db"),
				steveProject("c-abc", "p-idn", "p-1"), steveProject("c-abc", "p-idm", "p-1"), steveProject("c-def", "p-2", "cache"),
			})
			f := &fakeFactory{}
			_, _, err := ResolveProjectFlag(t.Context(), srv.getter(), f, "", tc.v)
			ce := codeOf(t, err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, []clierr.Candidate{{ID: "c-abc", Name: "Alpha"}}, ce.Candidates)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Equal(t, []string{allProjectsPath, clustersPath}, srv.requests())
			assert.Empty(t, f.resolved)
			assert.Empty(t, f.clusters)
		})
	}
}

func TestResolveProjectFlagWithoutClusterListsEveryMatchingClusterSorted(t *testing.T) {
	t.Parallel()
	srv := clustersAndProjects(t, []map[string]any{
		steveProject("c-ghi", "p-3", "web"), steveProject("c-def", "p-2", "web"), steveProject("c-abc", "p-1", "other"),
		steveProject("c-abc", "web", "x"),
	})
	_, _, err := ResolveProjectFlag(t.Context(), srv.getter(), &fakeFactory{}, "", "web")
	ce := codeOf(t, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "--cluster is required with --project; projects with the id or display name 'web' are on clusters 'c-abc', 'c-def', 'c-ghi' - pass --cluster with one of them", ce.Message)
	assert.Equal(t, []clierr.Candidate{{ID: "c-abc", Name: "Alpha"}, {ID: "c-def", Name: "Beta"}, {ID: "c-ghi", Name: "Gamma"}}, ce.Candidates)
	assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)
}

func TestResolveProjectFlagWithoutClusterAndNoMatchDoesNotReadClusters(t *testing.T) {
	t.Parallel()
	srv := clustersAndProjects(t, []map[string]any{steveProject("c-abc", "p-1", "db")})
	_, _, err := ResolveProjectFlag(t.Context(), srv.getter(), &fakeFactory{}, "", "web")
	ce := codeOf(t, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "--cluster is required with --project; no project you can read has the id or display name 'web'", ce.Message)
	assert.Equal(t, clierr.Rancher+" access projects", ce.Hint)
	assert.Empty(t, ce.Candidates)
	assert.Equal(t, []string{allProjectsPath}, srv.requests(), "the clusters collection is not read")
}

func TestResolveProjectFlagWithoutClusterReturnsAProjectsReadError(t *testing.T) {
	t.Parallel()
	srv := newServer(t, map[string]route{allProjectsPath: {status: 500}})
	_, _, err := ResolveProjectFlag(t.Context(), srv.getter(), &fakeFactory{}, "", "web")
	assert.Equal(t, clierr.CodeGeneric, codeOf(t, err).Code)
	require.ErrorContains(t, err, "answered 500")
	assert.Equal(t, []string{allProjectsPath}, srv.requests())
}

func TestResolveProjectFlagWithClusterResolvesItThenTheProject(t *testing.T) {
	t.Parallel()
	srv := newServer(t, map[string]route{abcProjectsPath: {pages: onePage(steveProject("c-abc", "p-1", "web"))}})
	local := &fakeCluster{}
	f := &fakeFactory{resolveTo: "c-abc", client: local}
	p, warning, err := ResolveProjectFlag(t.Context(), srv.getter(), f, "Alpha", "web")
	require.NoError(t, err)
	assert.Equal(t, Project{Cluster: "c-abc", ID: "p-1", DisplayName: "web"}, p)
	assert.Empty(t, warning)
	assert.Equal(t, []string{"Alpha"}, f.resolved)
	assert.Equal(t, []string{engine.LocalCluster}, f.clusters)
	assert.Equal(t, []string{abcProjectsPath}, srv.requests(), "one cluster's projects, no clusters read")
}

func TestResolveProjectFlagWithClusterFallsBackThroughTheLocalClient(t *testing.T) {
	t.Parallel()
	srv := newServer(t, map[string]route{abcProjectsPath: {pages: onePage()}})
	local := &fakeCluster{getErr: clierr.New(clierr.CodeNotFound, "gone")}
	f := &fakeFactory{resolveTo: "c-abc", client: local}
	_, _, err := ResolveProjectFlag(t.Context(), srv.getter(), f, "c-abc", "web")
	assert.Equal(t, clierr.CodeNotFound, codeOf(t, err).Code)
	require.Len(t, local.gets, 1)
	assert.Equal(t, "c-abc", local.gets[0].ns)
	assert.Equal(t, "web", local.gets[0].name)
}

func TestResolveProjectFlagWithClusterReturnsFactoryErrorsBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	boom := clierr.New(clierr.CodeNotFound, "no such cluster")
	srv := newServer(t, nil)
	f := &fakeFactory{resolveErr: boom}
	_, _, err := ResolveProjectFlag(t.Context(), srv.getter(), f, "nope", "web")
	assert.Same(t, boom, err)
	assert.Empty(t, f.clusters)
	assert.Empty(t, srv.requests())

	down := errors.New("no local client")
	f = &fakeFactory{resolveTo: "c-abc", clusterErr: down}
	_, _, err = ResolveProjectFlag(t.Context(), srv.getter(), f, "c-abc", "web")
	assert.Same(t, down, err)
	assert.Empty(t, srv.requests())
}
