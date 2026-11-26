package steve

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// cutSizes are the sizes of the lists the tests below give a lookup: exactly
// the limit, one more, and hundreds of objects.
var cutSizes = []int{clierr.CandidateLimit, clierr.CandidateLimit + 1, 300}

// requireFirstByID asserts ce lists the first clierr.CandidateLimit of n
// objects whose ids are prefix and a three-digit number, in the order of the
// ids, and records n as the total only when the list is longer than the
// limit.
func requireFirstByID(t *testing.T, ce *clierr.Error, prefix string, n int) {
	t.Helper()
	require.Len(t, ce.Candidates, min(n, clierr.CandidateLimit))
	for i, c := range ce.Candidates {
		assert.Equal(t, fmt.Sprintf("%s%03d", prefix, i), c.ID)
	}
	if n > clierr.CandidateLimit {
		assert.Equal(t, n, ce.CandidatesTotal)
	} else {
		assert.Zero(t, ce.CandidatesTotal)
	}
}

// projectsOnC returns n projects of cluster c-abc with ids p-000 and up,
// listed in descending order of their ids.
func projectsOnC(n int) []map[string]any {
	items := make([]map[string]any, 0, n)
	for i := n - 1; i >= 0; i-- {
		items = append(items, steveProject("c-abc", fmt.Sprintf("p-%03d", i), fmt.Sprintf("name-%03d", i)))
	}
	return items
}

// TestProjectLookupCutsItsCandidates asserts the NOT_FOUND error of a project
// lookup, and the RBAC error that replaces it when Rancher refuses the get by
// id, list the first clierr.CandidateLimit projects by id and the size of the
// full list, for ResolveProject and ResolveProjectArgument.
func TestProjectLookupCutsItsCandidates(t *testing.T) {
	t.Parallel()
	resolvers := map[string]func(*testing.T, Getter, *fakeCluster) error{
		"ResolveProject": func(t *testing.T, get Getter, local *fakeCluster) error {
			t.Helper()
			_, _, err := ResolveProject(t.Context(), get, local, "c-abc", "p-x")
			return err
		},
		"ResolveProjectArgument": func(t *testing.T, get Getter, local *fakeCluster) error {
			t.Helper()
			_, _, err := ResolveProjectArgument(t.Context(), get, local, "c-abc", "p-x")
			return err
		},
	}
	for name, resolve := range resolvers {
		for _, n := range cutSizes {
			t.Run(name+" "+strconv.Itoa(n)+" 404", func(t *testing.T) {
				t.Parallel()
				_, get := projectFixture(t, projectsOnC(n)...)
				err := resolve(t, get, &fakeCluster{getErr: projectRefusalFrom(http.StatusNotFound)})
				ce := codeOf(t, err)
				assert.Equal(t, clierr.CodeNotFound, ce.Code)
				requireFirstByID(t, ce, "p-", n)
			})
			t.Run(name+" "+strconv.Itoa(n)+" 403", func(t *testing.T) {
				t.Parallel()
				_, get := projectFixture(t, projectsOnC(n)...)
				err := resolve(t, get, &fakeCluster{getErr: projectRefusalFrom(http.StatusForbidden)})
				ce := codeOf(t, err)
				assert.Equal(t, clierr.CodeRBAC, ce.Code)
				requireFirstByID(t, ce, "p-", n)
			})
		}
	}
}

// clustersOn returns n clusters with ids c-000 and up, in descending order of
// their ids.
func clustersOn(n int) []Cluster {
	list := make([]Cluster, 0, n)
	for i := n - 1; i >= 0; i-- {
		list = append(list, Cluster{ID: fmt.Sprintf("c-%03d", i), DisplayName: fmt.Sprintf("name-%03d", i)})
	}
	return list
}

// TestClusterLookupCutsItsCandidates asserts the NOT_FOUND error of a cluster
// lookup, and the RBAC error that replaces it when Rancher refuses the get by
// id, list the first clierr.CandidateLimit clusters by id and the size of the
// full list.
func TestClusterLookupCutsItsCandidates(t *testing.T) {
	t.Parallel()
	byID := clusterByIDPath("c-new")
	for _, n := range cutSizes {
		list := clustersOn(n)
		t.Run("MatchCluster "+strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			_, _, err := MatchCluster(list, "c-new", cliText("c-new"))
			ce := codeOf(t, err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			requireFirstByID(t, ce, "c-", n)
		})
		t.Run("ResolveCluster "+strconv.Itoa(n)+" 404", func(t *testing.T) {
			t.Parallel()
			srv := newServer(t, map[string]route{byID: {status: http.StatusNotFound}})
			_, _, err := ResolveCluster(t.Context(), srv.getter(), list, "c-new", cliText("c-new"))
			ce := codeOf(t, err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			requireFirstByID(t, ce, "c-", n)
		})
		t.Run("ResolveCluster "+strconv.Itoa(n)+" 403", func(t *testing.T) {
			t.Parallel()
			srv := newServer(t, map[string]route{byID: {status: http.StatusForbidden}})
			_, _, err := ResolveCluster(t.Context(), srv.getter(), list, "c-new", cliText("c-new"))
			ce := codeOf(t, err)
			assert.Equal(t, clierr.CodeRBAC, ce.Code)
			requireFirstByID(t, ce, "c-", n)
		})
	}
}

// TestProjectFlagWithoutClusterCutsItsCandidates asserts the refusal of a
// --project typed without --cluster lists the first clierr.CandidateLimit
// clusters that have the project, and the size of the full list, and that the
// message names every cluster only while the list is within the limit: past
// it, the message gives the number of clusters.
func TestProjectFlagWithoutClusterCutsItsCandidates(t *testing.T) {
	t.Parallel()
	for _, n := range cutSizes {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			var projects []map[string]any
			var clusters []map[string]any
			for i := n - 1; i >= 0; i-- {
				id := fmt.Sprintf("c-%03d", i)
				projects = append(projects, steveProject(id, "p-default", "Default"))
				clusters = append(clusters, steveCluster(id, fmt.Sprintf("name-%03d", i)))
			}
			srv := newServer(t, map[string]route{
				allProjectsPath: {pages: onePage(projects...)},
				clustersPath:    {pages: onePage(clusters...)},
			})
			_, _, err := ResolveProjectFlag(t.Context(), srv.getter(), &fakeFactory{}, "", "Default")
			ce := codeOf(t, err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			requireFirstByID(t, ce, "c-", n)
			where := fmt.Sprintf("%d clusters (the first %d by id are in candidates)", n, clierr.CandidateLimit)
			if n <= clierr.CandidateLimit {
				var quoted []string
				for _, c := range ce.Candidates {
					quoted = append(quoted, "'"+c.ID+"'")
				}
				where = "clusters " + strings.Join(quoted, ", ")
			}
			assert.Equal(t, "--cluster is required with --project; projects with the id or display name 'Default' are on "+where+" - pass --cluster with one of them", ce.Message)
		})
	}
}

// candidateIDs returns the ids of the candidates of ce, in order.
func candidateIDs(ce *clierr.Error) []string {
	var out []string
	for _, c := range ce.Candidates {
		out = append(out, c.ID)
	}
	return out
}

// firstIDs is the ids a list of 300 entries named with a three-digit number
// lists for the typed value "web" when entries 100 and 250 contain it: those
// two first, then the others by id.
var firstIDs = []string{"100", "250", "000", "001", "002", "003", "004", "005", "006", "007"}

// withPrefix returns each of ids with prefix before it.
func withPrefix(prefix string, ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = prefix + id
	}
	return out
}

// TestProjectLookupListsTheMatchesFirst asserts the NOT_FOUND error of a
// project lookup, and the RBAC error that replaces it when Rancher refuses the
// get by id, list the projects whose id or name contains the typed value
// before the others, each part by id.
func TestProjectLookupListsTheMatchesFirst(t *testing.T) {
	t.Parallel()
	items := make([]map[string]any, 0, 300)
	for i := 299; i >= 0; i-- {
		name := fmt.Sprintf("name-%03d", i)
		switch i {
		case 250:
			name = "my-WEB"
		case 100:
			name = "webapp"
		}
		items = append(items, steveProject("c-abc", fmt.Sprintf("p-%03d", i), name))
	}
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			_, get := projectFixture(t, items...)
			_, _, err := ResolveProject(t.Context(), get, &fakeCluster{getErr: projectRefusalFrom(status)}, "c-abc", "web")
			ce := codeOf(t, err)
			assert.Equal(t, withPrefix("p-", firstIDs), candidateIDs(ce))
			assert.Equal(t, 300, ce.CandidatesTotal)
		})
	}
}

// TestClusterLookupListsTheMatchesFirst asserts the NOT_FOUND error of a
// cluster lookup, and the RBAC error that replaces it when Rancher refuses the
// get by id, list the clusters whose id or name contains the typed value before
// the others, each part by id.
func TestClusterLookupListsTheMatchesFirst(t *testing.T) {
	t.Parallel()
	list := make([]Cluster, 0, 300)
	for i := 299; i >= 0; i-- {
		name := fmt.Sprintf("name-%03d", i)
		switch i {
		case 250:
			name = "my-WEB"
		case 100:
			name = "webapp"
		}
		list = append(list, Cluster{ID: fmt.Sprintf("c-%03d", i), DisplayName: name})
	}
	_, _, err := MatchCluster(list, "web", cliText("web"))
	ce := codeOf(t, err)
	assert.Equal(t, withPrefix("c-", firstIDs), candidateIDs(ce))
	assert.Equal(t, 300, ce.CandidatesTotal)

	srv := newServer(t, map[string]route{clusterByIDPath("web"): {status: http.StatusForbidden}})
	_, _, err = ResolveCluster(t.Context(), srv.getter(), list, "web", cliText("web"))
	ce = codeOf(t, err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, withPrefix("c-", firstIDs), candidateIDs(ce))
	assert.Equal(t, 300, ce.CandidatesTotal)
}

// TestProjectFlagWithoutClusterListsTheClustersByID asserts the refusal of a
// --project typed without --cluster lists the clusters by id: every one of them
// has the project; a cluster whose name contains the typed value does not come
// first.
func TestProjectFlagWithoutClusterListsTheClustersByID(t *testing.T) {
	t.Parallel()
	var projects, clusters []map[string]any
	for i := 299; i >= 0; i-- {
		id := fmt.Sprintf("c-%03d", i)
		name := fmt.Sprintf("name-%03d", i)
		if i == 200 {
			name = "Default-prod"
		}
		projects = append(projects, steveProject(id, "p-default", "Default"))
		clusters = append(clusters, steveCluster(id, name))
	}
	srv := newServer(t, map[string]route{
		allProjectsPath: {pages: onePage(projects...)},
		clustersPath:    {pages: onePage(clusters...)},
	})
	_, _, err := ResolveProjectFlag(t.Context(), srv.getter(), &fakeFactory{}, "", "Default")
	ce := codeOf(t, err)
	assert.Equal(t, []string{"c-000", "c-001", "c-002", "c-003", "c-004", "c-005", "c-006", "c-007", "c-008", "c-009"}, candidateIDs(ce))
}
