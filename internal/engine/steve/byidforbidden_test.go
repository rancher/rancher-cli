package steve

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// byIDRefusal is the body of the Status that Kubernetes returns for a refused
// read.
const byIDRefusal = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"projects.management.cattle.io \"p-x\" is forbidden","reason":"Forbidden","code":403}`

// projectRefusalFrom is the error the local cluster client returns for a
// Status response with HTTP status status.
func projectRefusalFrom(status int) error {
	body := byIDRefusal
	if status == http.StatusNotFound {
		body = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"projects.management.cattle.io \"p-x\" not found","reason":"NotFound","code":404}`
	}
	return clierr.From(clierr.ResponseError(http.MethodGet, status, []byte(body)))
}

// requireRefusedByID asserts the RBAC error every lookup of a cluster or
// project returns when Rancher refuses (HTTP 403) the get by id: the error code and exit code,
// the message, the hint, that it is not retryable and the candidates.
func requireRefusedByID(t *testing.T, err error, message, hint string, candidates []clierr.Candidate) {
	t.Helper()
	require.Error(t, err)
	ce := codeOf(t, err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, 6, ce.ExitCode())
	assert.Equal(t, message, ce.Message)
	assert.Equal(t, hint, ce.Hint)
	assert.False(t, ce.Retryable)
	if len(candidates) == 0 {
		assert.Empty(t, ce.Candidates)
	} else {
		assert.Equal(t, candidates, ce.Candidates)
	}
	assert.Zero(t, ce.CandidatesTotal)
}

// TestClusterLookupsRefusedByIDAreRBAC asserts every function that gets one
// cluster by its id returns an RBAC error, after one request, when Rancher
// refuses (HTTP 403), and a NOT_FOUND error when Rancher responds with 404.
func TestClusterLookupsRefusedByIDAreRBAC(t *testing.T) {
	t.Parallel()
	const refusedMessage = "you cannot read cluster 'c-new' (GET /v1/management.cattle.io.clusters/c-new answered 403)"
	byID := clusterByIDPath("c-new")
	candidates := []clierr.Candidate{{ID: "c-abc", Name: "prod"}, {ID: "c-def", Name: "stage"}}
	hint := clierr.Rancher + " access clusters"

	resolve := func(list []Cluster, text func(string) engine.MatchText) func(*testing.T, Getter) error {
		return func(t *testing.T, get Getter) error {
			t.Helper()
			_, _, err := ResolveCluster(t.Context(), get, list, "c-new", text("c-new"))
			return err
		}
	}
	const (
		noReadable      = "you can read no cluster on this server; a Rancher administrator grants cluster and project roles"
		notFoundCLI     = "cluster not found: 'c-new'; it is not among the clusters you can read; pass a cluster id or display name"
		notFoundNone    = "cluster not found: 'c-new'; " + noReadable
		notFoundPlugin  = notFoundCLI + "; the clusters you can read: 'c-abc' (prod)"
		notFoundGetByID = "cluster not found: 'c-new'"
	)
	type outcome struct {
		message    string
		hint       string
		candidates []clierr.Candidate
	}
	rows := []struct {
		name     string
		call     func(*testing.T, Getter) error
		refused  outcome
		notFound outcome
	}{
		{"resolve with the CLI text", resolve(readable, cliText),
			outcome{refusedMessage, hint, candidates}, outcome{notFoundCLI, hint, candidates}},
		{"resolve with the CLI text and no readable cluster", resolve(nil, cliText),
			outcome{refusedMessage + "; " + noReadable, "", nil}, outcome{notFoundNone, "", nil}},
		{"resolve with the plugin text", resolve(readable, pluginText),
			outcome{refusedMessage + "; the clusters you can read: 'c-abc' (prod)", "", candidates}, outcome{notFoundPlugin, "", candidates}},
		{"resolve with the plugin text and no readable cluster", resolve(nil, func(v string) engine.MatchText {
			t := pluginText(v)
			t.RBACSuffix = ""
			return t
		}), outcome{refusedMessage + "; " + noReadable, "", nil}, outcome{notFoundNone, "", nil}},
		{"get a cluster", func(t *testing.T, get Getter) error {
			t.Helper()
			_, err := GetCluster(t.Context(), get, "c-new")
			return err
		}, outcome{refusedMessage, hint, nil}, outcome{notFoundGetByID, hint, nil}},
		{"get a cluster object", func(t *testing.T, get Getter) error {
			t.Helper()
			_, err := GetClusterObject(t.Context(), get, "c-new")
			return err
		}, outcome{refusedMessage, hint, nil}, outcome{notFoundGetByID, hint, nil}},
	}
	for _, row := range rows {
		t.Run(row.name+" 403", func(t *testing.T) {
			t.Parallel()
			srv := newServer(t, map[string]route{byID: {status: http.StatusForbidden}})
			err := row.call(t, srv.getter())
			requireRefusedByID(t, err, row.refused.message, row.refused.hint, row.refused.candidates)
			id, ok := RefusedClusterID(err)
			assert.True(t, ok)
			assert.Equal(t, "c-new", id)
			status, ok := clierr.HTTPStatus(err)
			assert.True(t, ok)
			assert.Equal(t, http.StatusForbidden, status)
			assert.Equal(t, []string{byID}, srv.requests())
		})
		t.Run(row.name+" 404", func(t *testing.T) {
			t.Parallel()
			srv := newServer(t, map[string]route{byID: {status: http.StatusNotFound}})
			err := row.call(t, srv.getter())
			ce := codeOf(t, err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Equal(t, row.notFound.message, ce.Message)
			assert.Equal(t, row.notFound.hint, ce.Hint)
			if len(row.notFound.candidates) == 0 {
				assert.Empty(t, ce.Candidates)
			} else {
				assert.Equal(t, row.notFound.candidates, ce.Candidates)
			}
			_, refused := RefusedClusterID(err)
			assert.False(t, refused)
			assert.Len(t, srv.requests(), 1)
		})
	}
}

// TestClusterCollectionRefusalIsNotRefusedByID asserts a 403 on the clusters
// collection read is RBAC but is not the error for a refused get by id.
func TestClusterCollectionRefusalIsNotRefusedByID(t *testing.T) {
	t.Parallel()
	srv := newServer(t, map[string]route{clustersPath: {status: http.StatusForbidden}})
	_, err := Clusters(t.Context(), srv.getter())
	require.Error(t, err)
	assert.Equal(t, clierr.CodeRBAC, codeOf(t, err).Code)
	_, ok := RefusedClusterID(err)
	assert.False(t, ok)
}

// TestClusterLookupRefusedByIDSameCandidatesAsTheNotFound asserts the RBAC
// error carries the candidates of the NOT_FOUND error MatchCluster returns for
// the same value and list.
func TestClusterLookupRefusedByIDSameCandidatesAsTheNotFound(t *testing.T) {
	t.Parallel()
	_, _, notFound := MatchCluster(readable, "c-new", cliText("c-new"))
	srv := newServer(t, map[string]route{clusterByIDPath("c-new"): {status: http.StatusForbidden}})
	_, _, err := ResolveCluster(t.Context(), srv.getter(), readable, "c-new", cliText("c-new"))
	want := codeOf(t, notFound)
	got := codeOf(t, err)
	assert.Equal(t, want.Candidates, got.Candidates)
	assert.Equal(t, want.CandidatesTotal, got.CandidatesTotal)
}

// TestClusterLookupNoByIDRequestForAValueThatIsNotAnID asserts that a value
// that cannot be a cluster id gives a NOT_FOUND error with no request,
// whatever response the server would send.
func TestClusterLookupNoByIDRequestForAValueThatIsNotAnID(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"my prod", "Prod"} {
		srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
		_, _, err := ResolveCluster(t.Context(), srv.getter(), readable, v, cliText(v))
		assert.Equal(t, clierr.CodeNotFound, codeOf(t, err).Code, v)
		_, refused := RefusedClusterID(err)
		assert.False(t, refused)
		assert.Empty(t, srv.requests(), v)
	}
}

// TestGetProjectKeepsTheStatusOfARefusedRead asserts that when Rancher refuses
// (HTTP 403) to get one project by its id the error is RBAC with HTTP status
// 403, and a 404 gives NOT_FOUND with HTTP status 404.
func TestGetProjectKeepsTheStatusOfARefusedRead(t *testing.T) {
	t.Parallel()
	path := Request{Type: ProjectsType, Namespace: "c-abc", Name: "p-x"}.Path()
	for status, code := range map[int]string{http.StatusForbidden: clierr.CodeRBAC, http.StatusNotFound: clierr.CodeNotFound} {
		srv := newServer(t, map[string]route{path: {status: status}})
		_, err := GetProject(t.Context(), srv.getter(), "c-abc", "p-x")
		require.Error(t, err)
		got, ok := clierr.HTTPStatus(err)
		assert.True(t, ok)
		assert.Equal(t, status, got)
		assert.Equal(t, code, clierr.From(err).Code)
		assert.Equal(t, []string{path}, srv.requests())
	}
}

// TestProjectLookupsRefusedByIDAreRBAC asserts ResolveProject and
// ResolveProjectArgument return an RBAC error when Rancher refuses (HTTP 403)
// the get of the project by its id, whether the caller can read some projects on the cluster or
// none, and return the NOT_FOUND error of the listing for a 404.
func TestProjectLookupsRefusedByIDAreRBAC(t *testing.T) {
	t.Parallel()
	resolvers := map[string]func(*testing.T, Getter, *fakeCluster, string) error{
		"ResolveProject": func(t *testing.T, get Getter, local *fakeCluster, v string) error {
			t.Helper()
			_, _, err := ResolveProject(t.Context(), get, local, "c-abc", v)
			return err
		},
		"ResolveProjectArgument": func(t *testing.T, get Getter, local *fakeCluster, v string) error {
			t.Helper()
			_, _, err := ResolveProjectArgument(t.Context(), get, local, "c-abc", v)
			return err
		},
	}
	lists := map[string][]map[string]any{
		"some projects": {steveProject("c-abc", "p-2", "db"), steveProject("c-abc", "p-1", "api")},
		"no project":    nil,
	}
	wantCandidates := map[string][]clierr.Candidate{
		"some projects": {{ID: "p-1", Name: "api"}, {ID: "p-2", Name: "db"}},
		"no project":    nil,
	}
	wantNotFoundHint := map[string]string{
		"some projects": clierr.Rancher + " access projects --cluster 'c-abc'",
		"no project":    "",
	}
	wantNotFound := map[string]string{
		"some projects": "project not found: 'p-x'; it is not among the projects you can read on cluster 'c-abc'; pass a project id or display name",
		"no project":    "project not found: 'p-x'; you can read no project on cluster 'c-abc'; a cluster owner or a Rancher administrator grants project roles",
	}
	for rname, resolve := range resolvers {
		for lname, items := range lists {
			t.Run(rname+" "+lname+" 403", func(t *testing.T) {
				t.Parallel()
				_, get := projectFixture(t, items...)
				local := &fakeCluster{getErr: projectRefusalFrom(http.StatusForbidden)}
				err := resolve(t, get, local, "p-x")
				requireRefusedByID(t, err, "you cannot read project 'p-x' on cluster 'c-abc'",
					clierr.Rancher+" access projects --cluster 'c-abc'", wantCandidates[lname])
				_, refused := RefusedClusterID(err)
				assert.False(t, refused, "a refused get of a project is not the error for a refused get of a cluster")
				status, ok := clierr.HTTPStatus(err)
				assert.True(t, ok)
				assert.Equal(t, http.StatusForbidden, status)
				assert.Len(t, local.gets, 1)
			})
			t.Run(rname+" "+lname+" 404", func(t *testing.T) {
				t.Parallel()
				_, get := projectFixture(t, items...)
				local := &fakeCluster{getErr: projectRefusalFrom(http.StatusNotFound)}
				err := resolve(t, get, local, "p-x")
				ce := codeOf(t, err)
				assert.Equal(t, clierr.CodeNotFound, ce.Code)
				assert.Equal(t, wantNotFound[lname], ce.Message)
				assert.Equal(t, wantNotFoundHint[lname], ce.Hint)
				if len(wantCandidates[lname]) == 0 {
					assert.Empty(t, ce.Candidates)
				} else {
					assert.Equal(t, wantCandidates[lname], ce.Candidates)
				}
				assert.Len(t, local.gets, 1)
			})
			t.Run(rname+" "+lname+" not an id", func(t *testing.T) {
				t.Parallel()
				_, get := projectFixture(t, items...)
				local := &fakeCluster{getErr: projectRefusalFrom(http.StatusForbidden)}
				err := resolve(t, get, local, "My Web")
				assert.Equal(t, clierr.CodeNotFound, codeOf(t, err).Code)
				assert.Empty(t, local.gets)
			})
		}
	}
}

// TestProjectLookupRefusedByIDLeavesOutTheHintForAClusterIDThatCannotBePrinted
// asserts that for a cluster id the shell would misread, the error has no hint
// and its message carries the note that the command is left out exactly once.
func TestProjectLookupRefusedByIDLeavesOutTheHintForAClusterIDThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	id := "c-'x"
	path, err := url.PathUnescape(Request{Type: ProjectsType, Namespace: id}.Path())
	require.NoError(t, err)
	srv := newServer(t, map[string]route{path: {pages: onePage()}})
	local := &fakeCluster{getErr: projectRefusalFrom(http.StatusForbidden)}
	_, _, err = ResolveProject(t.Context(), srv.getter(), local, id, "p-x")
	ce := codeOf(t, err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Empty(t, ce.Hint)
	assert.Equal(t, 1, strings.Count(ce.Message, clierr.LeftOutNote), ce.Message)
}
