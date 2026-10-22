package steve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// cliText and pluginText are the two shapes of MatchText the resolvers
// pass: a NOT_FOUND with a hint, and one that lists the clusters and has
// none.
func cliText(v string) engine.MatchText {
	return engine.MatchText{Noun: "cluster", Plural: "clusters", Flag: "--cluster",
		NotFound:     "cluster not found: " + clierr.ShellQuote(v) + "; it is not among the clusters you can read; pass a cluster id or display name",
		NotFoundHint: clierr.Rancher + " access clusters"}
}

func pluginText(v string) engine.MatchText {
	t := cliText(v)
	t.NotFound += "; the clusters you can read: 'c-abc' (prod)"
	t.NotFoundHint = ""
	return t
}

var readable = []Cluster{{ID: "c-abc", DisplayName: "prod"}, {ID: "c-def", DisplayName: "stage"}}

func clusterByIDPath(id string) string { return Request{Type: ClustersType, Name: id}.Path() }

// TestMatchClusterByIDOrDisplayName asserts both texts resolve an id and a
// unique display name and send no request.
func TestMatchClusterByIDOrDisplayName(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]func(string) engine.MatchText{"cli": cliText, "plugin": pluginText} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			id, warning, err := MatchCluster(readable, "c-def", text("c-def"))
			require.NoError(t, err)
			assert.Equal(t, "c-def", id)
			assert.Empty(t, warning)

			id, warning, err = MatchCluster(readable, "prod", text("prod"))
			require.NoError(t, err)
			assert.Equal(t, "c-abc", id)
			assert.Empty(t, warning)
		})
	}
}

// TestMatchClusterIDWinsWithAWarning asserts an id that is also another
// cluster's display name resolves to the id and names the other cluster.
func TestMatchClusterIDWinsWithAWarning(t *testing.T) {
	t.Parallel()
	list := []Cluster{{ID: "prod", DisplayName: "c-abc"}, {ID: "c-abc", DisplayName: "prod"}}
	id, warning, err := MatchCluster(list, "prod", cliText("prod"))
	require.NoError(t, err)
	assert.Equal(t, "prod", id)
	assert.Equal(t, "--cluster 'prod' is the id of cluster 'prod'; cluster 'c-abc' also has the display name 'prod' - pass --cluster 'c-abc' for that one", warning)
}

func TestMatchClusterDuplicateDisplayNameIsAmbiguous(t *testing.T) {
	t.Parallel()
	list := []Cluster{{ID: "c-3", DisplayName: "prod"}, {ID: "c-2", DisplayName: "other"}, {ID: "c-1", DisplayName: "prod"}}
	_, _, err := MatchCluster(list, "prod", cliText("prod"))
	ce := codeOf(t, err)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, "2 clusters have display name 'prod'; pass the cluster id", ce.Message)
	assert.Equal(t, []clierr.Candidate{{ID: "c-1", Name: "prod"}, {ID: "c-3", Name: "prod"}}, ce.Candidates)
}

// TestMatchClusterNotFoundUsesTheCallersText asserts each caller's
// NOT_FOUND text and hint, with every cluster as a candidate.
func TestMatchClusterNotFoundUsesTheCallersText(t *testing.T) {
	t.Parallel()
	_, _, err := MatchCluster(readable, "web", cliText("web"))
	ce := codeOf(t, err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "cluster not found: 'web'; it is not among the clusters you can read; pass a cluster id or display name", ce.Message)
	assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)
	assert.Equal(t, []clierr.Candidate{{ID: "c-abc", Name: "prod"}, {ID: "c-def", Name: "stage"}}, ce.Candidates)

	_, _, err = MatchCluster(readable, "web", pluginText("web"))
	ce = codeOf(t, err)
	assert.Equal(t, "cluster not found: 'web'; it is not among the clusters you can read; pass a cluster id or display name; the clusters you can read: 'c-abc' (prod)", ce.Message)
	assert.Empty(t, ce.Hint)
}

// TestMatchClusterEmptyListSaysYouCanReadNone asserts the empty-list text
// replaces either caller's text and hint.
func TestMatchClusterEmptyListSaysYouCanReadNone(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]func(string) engine.MatchText{"cli": cliText, "plugin": pluginText} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := MatchCluster(nil, "prod", text("prod"))
			ce := codeOf(t, err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Equal(t, "cluster not found: 'prod'; you can read no cluster on this server; a Rancher administrator grants cluster and project roles", ce.Message)
			assert.Empty(t, ce.Hint)
			assert.Empty(t, ce.Candidates)
		})
	}
}

// TestResolveClusterMatchSendsNoRequest asserts a list match never falls
// back to a GET.
func TestResolveClusterMatchSendsNoRequest(t *testing.T) {
	t.Parallel()
	srv := newServer(t, nil)
	id, _, err := ResolveCluster(t.Context(), srv.getter(), readable, "stage", cliText("stage"))
	require.NoError(t, err)
	assert.Equal(t, "c-def", id)
	assert.Empty(t, srv.requests())
}

// TestResolveClusterGetByIDOnAMiss asserts the one GET by id a miss sends:
// a 200 resolves to the value, a 403 or 404 keeps the NOT_FOUND, and any
// other status is returned.
func TestResolveClusterGetByIDOnAMiss(t *testing.T) {
	t.Parallel()
	for _, text := range []func(string) engine.MatchText{cliText, pluginText} {
		srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"id":"c-new","metadata":{"name":"c-new"},"spec":{"displayName":"new"}}`)
		})
		id, warning, err := ResolveCluster(t.Context(), srv.getter(), readable, "c-new", text("c-new"))
		require.NoError(t, err)
		assert.Equal(t, "c-new", id)
		assert.Empty(t, warning)
		assert.Equal(t, []string{clusterByIDPath("c-new")}, srv.requests())
	}

	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			srv := newServer(t, map[string]route{clusterByIDPath("c-new"): {status: status}})
			_, _, err := ResolveCluster(t.Context(), srv.getter(), readable, "c-new", cliText("c-new"))
			ce := codeOf(t, err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Equal(t, "cluster not found: 'c-new'; it is not among the clusters you can read; pass a cluster id or display name", ce.Message)
			assert.Len(t, srv.requests(), 1)
		})
	}

	t.Run("500", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t, map[string]route{clusterByIDPath("c-new"): {status: http.StatusInternalServerError}})
		_, _, err := ResolveCluster(t.Context(), srv.getter(), readable, "c-new", cliText("c-new"))
		ce := codeOf(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		require.ErrorContains(t, err, "rancher root GET "+clusterByIDPath("c-new"))
		se, ok := clierr.HTTPStatus(err)
		require.True(t, ok)
		assert.Equal(t, http.StatusInternalServerError, se)
	})

	t.Run("another cluster's object", func(t *testing.T) {
		t.Parallel()
		srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"id":"c-other","metadata":{"name":"c-other"}}`)
		})
		_, _, err := ResolveCluster(t.Context(), srv.getter(), readable, "c-new", cliText("c-new"))
		require.ErrorContains(t, err, `the answer is cluster "c-other"`)
	})
}

// TestResolveClusterNoGetForAValueThatIsNotAName asserts a display name
// with a space or capitals, which no cluster id can be, is NOT_FOUND with
// no GET.
func TestResolveClusterNoGetForAValueThatIsNotAName(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"my prod", "Prod", "c-abc/x"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			srv := newServer(t, nil)
			_, _, err := ResolveCluster(t.Context(), srv.getter(), readable, v, cliText(v))
			assert.Equal(t, clierr.CodeNotFound, codeOf(t, err).Code)
			assert.Empty(t, srv.requests())
		})
	}
}

// TestResolveClusterAmbiguousSendsNoGet asserts only a NOT_FOUND falls
// back to the GET by id.
func TestResolveClusterAmbiguousSendsNoGet(t *testing.T) {
	t.Parallel()
	srv := newServer(t, nil)
	list := []Cluster{{ID: "c-1", DisplayName: "prod"}, {ID: "c-2", DisplayName: "prod"}}
	_, _, err := ResolveCluster(t.Context(), srv.getter(), list, "prod", cliText("prod"))
	assert.Equal(t, clierr.CodeAmbiguousSelection, codeOf(t, err).Code)
	assert.Empty(t, srv.requests())
}

func TestGetClusterReadsOneObject(t *testing.T) {
	t.Parallel()
	srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"c-abc","metadata":{"name":"c-abc"},"spec":{"displayName":"prod"}}`)
	})
	obj, err := GetCluster(t.Context(), srv.getter(), "c-abc")
	require.NoError(t, err)
	assert.Equal(t, "c-abc", obj.GetName())
	assert.Equal(t, []string{clusterByIDPath("c-abc")}, srv.requests())
}

// TestGetCluster403IsRBACNamingTheRequest asserts the 403 text and hint,
// and that other failures, a 401 and a 500 among them, keep the getter's
// error.
func TestGetCluster403IsRBACNamingTheRequest(t *testing.T) {
	t.Parallel()
	srv := newServer(t, map[string]route{clusterByIDPath("c-abc"): {status: http.StatusForbidden}})
	_, err := GetCluster(t.Context(), srv.getter(), "c-abc")
	ce := codeOf(t, err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "you cannot read cluster 'c-abc' (GET /v1/management.cattle.io.clusters/c-abc answered 403)", ce.Message)
	assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)

	srv = newServer(t, map[string]route{clusterByIDPath("c-abc"): {status: http.StatusInternalServerError}})
	_, err = GetCluster(t.Context(), srv.getter(), "c-abc")
	require.ErrorContains(t, err, "rancher root GET "+clusterByIDPath("c-abc"))

	srv = newServer(t, map[string]route{clusterByIDPath("c-abc"): {status: http.StatusUnauthorized}})
	_, err = GetCluster(t.Context(), srv.getter(), "c-abc")
	require.ErrorContains(t, err, "rancher root GET "+clusterByIDPath("c-abc"))
	assert.NotEqual(t, clierr.CodeNotFound, codeOf(t, err).Code, "a 401 is not a missing cluster")

	srv = newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, `[]`) })
	_, err = GetCluster(t.Context(), srv.getter(), "c-abc")
	require.ErrorContains(t, err, "not a JSON object")
}

// TestGetCluster404IsNotFoundNamingTheCluster asserts that a 404, a
// cluster deleted after it was resolved, is NOT_FOUND naming the cluster
// and nothing more, with the access clusters hint.
func TestGetCluster404IsNotFoundNamingTheCluster(t *testing.T) {
	t.Parallel()
	srv := newServer(t, map[string]route{clusterByIDPath("c-gone"): {status: http.StatusNotFound}})
	_, err := GetCluster(t.Context(), srv.getter(), "c-gone")
	ce := codeOf(t, err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "cluster not found: 'c-gone'", ce.Message)
	assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)
}

// recordedClusterObservation reads the cluster object a Rancher v2.15.2
// served as the observation named name in the recorded golden, with every
// value that varies per run replaced by a placeholder.
func recordedClusterObservation(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testrancher", "fidelity", "testdata", "downstream",
		"access-downstream", "admin-steve-cluster-shape.json"))
	require.NoError(t, err)
	var golden struct {
		Observations []struct {
			Name   string         `json:"name"`
			Status int            `json:"status"`
			Body   map[string]any `json:"body"`
		} `json:"observations"`
	}
	require.NoError(t, json.Unmarshal(raw, &golden))
	for _, o := range golden.Observations {
		if o.Name == name {
			require.Equal(t, http.StatusOK, o.Status)
			return &unstructured.Unstructured{Object: o.Body}
		}
	}
	require.FailNow(t, "the golden has no observation named "+name)
	return nil
}

// TestObjectStateReadsTheRecordedSteveObject asserts the reader returns the
// state a Rancher served in metadata.state, and nothing for the object its
// Kubernetes proxy served, which has no metadata.state.
func TestObjectStateReadsTheRecordedSteveObject(t *testing.T) {
	t.Parallel()
	served := recordedClusterObservation(t, "steve")
	_, ok, err := unstructured.NestedMap(served.Object, "metadata", "state")
	require.NoError(t, err)
	require.True(t, ok, "the recorded steve object carries metadata.state")
	assert.Equal(t, "active", ObjectState(served))

	assert.Empty(t, ObjectState(recordedClusterObservation(t, "local")))
}

// TestObjectStateIgnoresStatusState asserts an object with only status.state
// has no state: steve serves the state in metadata.state.
func TestObjectStateIgnoresStatusState(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-abc"},
		"status":   map[string]any{"state": "active"},
	}}
	assert.Empty(t, ObjectState(obj))
	assert.Empty(t, ObjectState(nil))
}

// TestObjectStateMalformedIsEmpty asserts a metadata.state that is not a map,
// or whose name is not a string, reads as empty.
func TestObjectStateMalformedIsEmpty(t *testing.T) {
	t.Parallel()
	for name, state := range map[string]any{
		"not a map":        "active",
		"name not string":  map[string]any{"name": 7},
		"name absent":      map[string]any{"message": "Resource is Ready"},
		"state is null":    nil,
		"name is a map":    map[string]any{"name": map[string]any{"x": "y"}},
		"name is null":     map[string]any{"name": nil},
		"state is a slice": []any{"active"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			obj := &unstructured.Unstructured{Object: map[string]any{
				"metadata": map[string]any{"name": "c-abc", "state": state},
			}}
			assert.NotPanics(t, func() { assert.Empty(t, ObjectState(obj)) })
		})
	}
}
