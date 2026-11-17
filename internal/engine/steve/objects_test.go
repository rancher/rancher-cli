package steve

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedBody reads the observation called name from file, a recorded
// access-downstream golden in the fidelity testdata, replaces the placeholders
// for the downstream cluster and the project with ids, and returns its status
// and body.
func recordedBody(t *testing.T, file, name string) (int, map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testrancher", "fidelity", "testdata", "downstream", "access-downstream", file))
	require.NoError(t, err)
	text := strings.NewReplacer("{downstream}", "c-m-abc", "{project}", "p-abc").Replace(string(raw))
	var golden struct {
		Observations []struct {
			Name   string         `json:"name"`
			Status int            `json:"status"`
			Body   map[string]any `json:"body"`
		} `json:"observations"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &golden))
	for _, o := range golden.Observations {
		if o.Name == name {
			return o.Status, o.Body
		}
	}
	require.FailNow(t, "the golden "+file+" has no observation named "+name)
	return 0, nil
}

// TestKubernetesObjectMatchesTheKubernetesRead asserts the conversion of the
// cluster Steve returned in the recorded golden is the object the Kubernetes
// API returned for the same cluster.
func TestKubernetesObjectMatchesTheKubernetesRead(t *testing.T) {
	t.Parallel()
	_, steveBody := recordedBody(t, "admin-steve-cluster-shape.json", "steve")
	_, local := recordedBody(t, "admin-steve-cluster-shape.json", "local")
	assert.Equal(t, local, KubernetesObject(steveBody))
}

// steveClusterWithEverything is a Steve cluster that holds every field the
// conversion removes or renames, and fields it keeps.
func steveClusterWithEverything() map[string]any {
	return map[string]any{
		"id": "c-m-abc", "type": "management.cattle.io.cluster", "kind": "Cluster", "apiVersion": "management.cattle.io/v3",
		"links": map[string]any{"self": "https://rancher.invalid/v1/x"}, "actions": map[string]any{"a": "b"},
		"_id": "own-id", "_type": "own-type", "_links": "own-links", "_actions": "own-actions",
		"metadata": map[string]any{
			"name": "c-m-abc", "labels": map[string]any{"team": "a"},
			"fields": []any{"c-m-abc", "Active"}, "state": map[string]any{"name": "active"}, "relationships": []any{map[string]any{"toId": "x"}},
		},
		"spec": map[string]any{"displayName": "prod"},
		"status": map[string]any{
			"connected":   true,
			"requested":   map[string]any{"cpu": "1", "cpuRaw": float64(1), "memory": "1Gi", "memoryRaw": float64(1073741824)},
			"allocatable": map[string]any{"cpu": "4", "cpuRaw": float64(4), "memory": "8Gi", "memoryRaw": float64(8589934592), "pods": "110"},
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True", "error": false, "transitioning": false, "lastUpdateTime": "2026-01-01T00:00:00Z"},
				map[string]any{"type": "Updated", "status": "False", "error": true, "transitioning": true, "message": "m"},
			},
		},
	}
}

// steveSingleCluster is steveClusterWithEverything as Steve returns one object
// by id: with id and without _id, which only a collection item has, and so with
// no field Steve renamed.
func steveSingleCluster() map[string]any {
	in := steveClusterWithEverything()
	for _, k := range []string{"_id", "_type", "_links", "_actions"} {
		delete(in, k)
	}
	return in
}

// TestKubernetesObjectRemovesWhatSteveAdds asserts each field Steve adds is
// gone, a top-level field Steve renamed is renamed back, every other field
// stays, and the input is never modified.
func TestKubernetesObjectRemovesWhatSteveAdds(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in  map[string]any
		own map[string]any
	}{
		"collection item":  {steveClusterWithEverything(), map[string]any{"id": "own-id", "type": "own-type", "links": "own-links", "actions": "own-actions"}},
		"one object by id": {steveSingleCluster(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertConvertsSteveCluster(t, tc.in, tc.own)
		})
	}

	// Nothing but a Cluster loses status.connected or the raw resource values.
	project := map[string]any{"kind": "Project", "id": "c/p", "status": map[string]any{"connected": "own", "requested": map[string]any{"cpuRaw": "own"}}}
	assert.Equal(t, map[string]any{"kind": "Project", "status": map[string]any{"connected": "own", "requested": map[string]any{"cpuRaw": "own"}}}, KubernetesObject(project))
}

// assertConvertsSteveCluster asserts the conversion of in removes every field
// Steve adds, leaves the fields in own as the fields of the object, keeps every
// other field, and never modifies in.
func assertConvertsSteveCluster(t *testing.T, in, own map[string]any) {
	t.Helper()
	before := deepCopy(in)
	got := KubernetesObject(in)
	assert.Equal(t, before, in, "the input is never modified")

	want := map[string]any{
		"kind": "Cluster", "apiVersion": "management.cattle.io/v3",
		"metadata": map[string]any{"name": "c-m-abc", "labels": map[string]any{"team": "a"}},
		"spec":     map[string]any{"displayName": "prod"},
		"status": map[string]any{
			"requested":   map[string]any{"cpu": "1", "memory": "1Gi"},
			"allocatable": map[string]any{"cpu": "4", "memory": "8Gi", "pods": "110"},
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True", "lastUpdateTime": "2026-01-01T00:00:00Z"},
				map[string]any{"type": "Updated", "status": "False", "message": "m"},
			},
		},
	}
	maps.Copy(want, own)
	assert.Equal(t, want, got)
}

// TestKubernetesObjectRemovesDuplicatesOfSteveFields asserts a top-level _id,
// _type, _links or _actions with the same value as the field Steve adds under
// the name without the underscore is removed (the duplicate in the list
// response of Steve's cache), and one with another value is the object's own
// field and is renamed back.
func TestKubernetesObjectRemovesDuplicatesOfSteveFields(t *testing.T) {
	t.Parallel()
	in := map[string]any{
		"id": "c-m-abc", "_id": "c-m-abc",
		"type": "management.cattle.io.cluster", "_type": "management.cattle.io.cluster",
		"links": map[string]any{"self": "x"}, "_links": map[string]any{"self": "x"},
		"actions": map[string]any{}, "_actions": "own-actions",
		"metadata": map[string]any{"name": "c-m-abc"},
	}
	assert.Equal(t, map[string]any{"actions": "own-actions", "metadata": map[string]any{"name": "c-m-abc"}}, KubernetesObject(in))
}

// TestKubernetesObjectRemovesAnEmptyLastUpdateTime asserts a condition's
// lastUpdateTime is removed when it is the empty string, as Steve writes it
// when the condition has no time, and kept when it holds a time.
func TestKubernetesObjectRemovesAnEmptyLastUpdateTime(t *testing.T) {
	t.Parallel()
	in := map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "A", "status": "True", "lastUpdateTime": ""},
		map[string]any{"type": "B", "status": "True", "lastUpdateTime": "2026-01-01T00:00:00Z"},
		map[string]any{"type": "C", "status": "True"},
	}}}
	want := map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "A", "status": "True"},
		map[string]any{"type": "B", "status": "True", "lastUpdateTime": "2026-01-01T00:00:00Z"},
		map[string]any{"type": "C", "status": "True"},
	}}}
	assert.Equal(t, want, KubernetesObject(in))
}

// TestKubernetesObjectRemovesIDAndTypeFromAProject asserts a project item
// the recorded collection holds loses id and type and keeps its namespace.
func TestKubernetesObjectRemovesIDAndTypeFromAProject(t *testing.T) {
	t.Parallel()
	_, body := recordedBody(t, "member-steve-projects-by-cluster.json", "projects")
	items, ok := body["data"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	item, ok := items[0].(map[string]any)
	require.True(t, ok)
	got := KubernetesObject(item)
	assert.NotContains(t, got, "id")
	assert.NotContains(t, got, "type")
	assert.Equal(t, "c-m-abc", got["metadata"].(map[string]any)["namespace"])
	assert.Equal(t, "p-abc", got["metadata"].(map[string]any)["name"])
}

func richCluster(id, display string, labels map[string]any) map[string]any {
	return map[string]any{"id": id, "type": "management.cattle.io.cluster", "kind": "Cluster",
		"metadata": map[string]any{"name": id, "labels": labels}, "spec": map[string]any{"displayName": display}}
}

func richProject(cluster, id, display string) map[string]any {
	return map[string]any{"id": cluster + "/" + id, "type": "management.cattle.io.project", "kind": "Project",
		"metadata": map[string]any{"name": id, "namespace": cluster}, "spec": map[string]any{"clusterName": cluster, "displayName": display}}
}

// TestClusterObjectsKeepsEachItemAsSteveReturnsIt asserts the items from the
// clusters collection come back as Steve returned them, in the order Steve
// returned them.
func TestClusterObjectsKeepsEachItemAsSteveReturnsIt(t *testing.T) {
	t.Parallel()
	b := richCluster("c-b", "Beta", map[string]any{"team": "a"})
	a := richCluster("c-a", "Alpha", nil)
	srv := clustersServer(t, onePage(b, a))
	got, err := ClusterObjects(t.Context(), srv.getter())
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "c-b", got[0].GetName())
	assert.Equal(t, "c-a", got[1].GetName())
	assert.Equal(t, "management.cattle.io.cluster", got[0].Object["type"], "the Steve fields are kept: the caller converts the objects")
	assert.Equal(t, map[string]string{"team": "a"}, got[0].GetLabels())
	assert.Equal(t, []string{clustersPath}, srv.requests())
}

// TestClusterObjectsReturnsAProtocolErrorForAMissingOrMismatchedNameOrID
// asserts an item with no metadata.name, with a metadata.name that is not its
// id and with no id is a protocol error, and that an empty collection returns
// no clusters.
func TestClusterObjectsReturnsAProtocolErrorForAMissingOrMismatchedNameOrID(t *testing.T) {
	t.Parallel()
	for name, item := range map[string]map[string]any{
		"no name":          {"id": "c-a", "spec": map[string]any{}},
		"other name":       {"id": "c-a", "metadata": map[string]any{"name": "c-b"}},
		"no id":            {"metadata": map[string]any{"name": "c-a"}},
		"empty name":       {"id": "c-a", "metadata": map[string]any{"name": ""}},
		"name is a number": {"id": "c-a", "metadata": map[string]any{"name": float64(1)}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := clustersServer(t, onePage(item))
			_, err := ClusterObjects(t.Context(), srv.getter())
			require.ErrorContains(t, err, "unexpected response from "+clustersPath)
		})
	}
	srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[],"revision":"1","type":"collection"}`)
	})
	got, err := ClusterObjects(t.Context(), srv.getter())
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestClusterObjectsRefusalsKeepTheirStatus asserts a 403 and a 404 are
// worded as the other collection readers word them and still carry their
// status for clierr.HTTPStatus.
func TestClusterObjectsRefusalsKeepTheirStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		srv := newServer(t, map[string]route{clustersPath: {status: status}})
		_, err := ClusterObjects(t.Context(), srv.getter())
		require.Error(t, err)
		got, ok := clierr.HTTPStatus(err)
		assert.True(t, ok, "status %d", status)
		assert.Equal(t, status, got)
	}
}

// TestProjectObjectsReadsOneClusterOrEvery asserts the request path of each
// read and that an item keeps its namespace.
func TestProjectObjectsReadsOneClusterOrEvery(t *testing.T) {
	t.Parallel()
	srv := newServer(t, map[string]route{
		allProjectsPath: {pages: onePage(richProject("c-b", "p-2", "B"), richProject("c-a", "p-1", "A"))},
		abcProjectsPath: {pages: onePage(richProject("c-abc", "p-9", "C"))},
	})
	all, err := ProjectObjects(t.Context(), srv.getter(), "")
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, "c-b", all[0].GetNamespace())
	assert.Equal(t, "p-2", all[0].GetName())
	one, err := ProjectObjects(t.Context(), srv.getter(), "c-abc")
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, "p-9", one[0].GetName())
	assert.Equal(t, []string{allProjectsPath, abcProjectsPath}, srv.requests())
}

// TestProjectObjectsReturnsAProtocolErrorForAMissingNameOrNamespaceOrAnotherCluster
// asserts an item with no metadata.name or no metadata.namespace is a protocol
// error, and so is an item on a cluster other than the one requested.
func TestProjectObjectsReturnsAProtocolErrorForAMissingNameOrNamespaceOrAnotherCluster(t *testing.T) {
	t.Parallel()
	noName := richProject("c-abc", "p-1", "A")
	delete(noName["metadata"].(map[string]any), "name")
	noNamespace := richProject("c-abc", "p-1", "A")
	delete(noNamespace["metadata"].(map[string]any), "namespace")
	otherCluster := richProject("c-other", "p-1", "A")
	for name, item := range map[string]map[string]any{"no name": noName, "no namespace": noNamespace, "other cluster": otherCluster} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(t, map[string]route{abcProjectsPath: {pages: onePage(item)}})
			_, err := ProjectObjects(t.Context(), srv.getter(), "c-abc")
			require.ErrorContains(t, err, "unexpected response from "+abcProjectsPath)
		})
	}
}

// TestProjectObjectsRefusalsKeepTheirStatus asserts a refused projects read
// carries its status.
func TestProjectObjectsRefusalsKeepTheirStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		srv := newServer(t, map[string]route{abcProjectsPath: {status: status}})
		_, err := ProjectObjects(t.Context(), srv.getter(), "c-abc")
		got, ok := clierr.HTTPStatus(err)
		assert.True(t, ok, "status %d", status)
		assert.Equal(t, status, got)
	}
}

func projectByIDPath(cluster, id string) string {
	return Request{Type: ProjectsType, Namespace: cluster, Name: id}.Path()
}

// TestGetProjectReadsOneObject asserts the recorded project response is read
// from the path of the project by id and returned as Steve returned it.
func TestGetProjectReadsOneObject(t *testing.T) {
	t.Parallel()
	status, body := recordedBody(t, "member-steve-projects-by-cluster.json", "get-own-project")
	require.Equal(t, http.StatusOK, status)
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(raw) })
	obj, err := GetProject(t.Context(), srv.getter(), "c-m-abc", "p-abc")
	require.NoError(t, err)
	assert.Equal(t, "p-abc", obj.GetName())
	assert.Equal(t, "c-m-abc", obj.GetNamespace())
	assert.Equal(t, []string{projectByIDPath("c-m-abc", "p-abc")}, srv.requests())
}

// TestGetProjectReturnsAProtocolErrorForAResponseItCannotTrust asserts a 2xx
// body that is not an object, or does not name the project and cluster
// requested, is a protocol error, and a non-2xx response keeps its status.
func TestGetProjectReturnsAProtocolErrorForAResponseItCannotTrust(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"not an object":   `[]`,
		"not json":        `nope`,
		"no metadata":     `{"kind":"Project"}`,
		"no name":         `{"metadata":{"namespace":"c-abc"}}`,
		"no namespace":    `{"metadata":{"name":"p-1"}}`,
		"other name":      `{"metadata":{"name":"p-2","namespace":"c-abc"}}`,
		"other namespace": `{"metadata":{"name":"p-1","namespace":"c-other"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) })
			_, err := GetProject(t.Context(), srv.getter(), "c-abc", "p-1")
			require.ErrorContains(t, err, "unexpected response from "+projectByIDPath("c-abc", "p-1"))
		})
	}
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		srv := newServer(t, map[string]route{projectByIDPath("c-abc", "p-1"): {status: status}})
		_, err := GetProject(t.Context(), srv.getter(), "c-abc", "p-1")
		got, ok := clierr.HTTPStatus(err)
		assert.True(t, ok, "status %d", status)
		assert.Equal(t, status, got)
	}
}

// TestGetClusterObjectReturnsAProtocolErrorForABodyThatDoesNotNameTheCluster
// asserts the object is returned as Steve returned it and a body with no
// metadata.name, or another one, is a protocol error.
func TestGetClusterObjectReturnsAProtocolErrorForABodyThatDoesNotNameTheCluster(t *testing.T) {
	t.Parallel()
	serve := func(body string) *recordingServer {
		return newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) })
	}
	obj, err := GetClusterObject(t.Context(), serve(`{"id":"c-abc","metadata":{"name":"c-abc","state":{}},"spec":{"displayName":"prod"}}`).getter(), "c-abc")
	require.NoError(t, err)
	assert.Contains(t, obj.Object["metadata"], "state", "the object is returned as Steve returned it")

	for name, body := range map[string]string{
		"no name":    `{"id":"c-abc","spec":{}}`,
		"other name": `{"id":"c-abc","metadata":{"name":"c-def"}}`,
	} {
		_, err := GetClusterObject(t.Context(), serve(body).getter(), "c-abc")
		require.ErrorContains(t, err, "unexpected response from "+clusterByIDPath("c-abc"), name)
	}

	srv := newServer(t, map[string]route{clusterByIDPath("c-gone"): {status: http.StatusNotFound}})
	_, err = GetClusterObject(t.Context(), srv.getter(), "c-gone")
	assert.True(t, clierr.IsNotFound(err))
}

// TestItemThatIsNotAnObjectNamesItsJSONType asserts the decoder of a collection
// item that is not a JSON object names the type of the item, and that the
// project decoder reports an object whose field has the wrong type.
func TestItemThatIsNotAnObjectNamesItsJSONType(t *testing.T) {
	t.Parallel()
	const index = 3
	accept := func(string, int, json.RawMessage) (string, string, error) { return "x", "x", nil }
	for _, tc := range []struct{ class, item string }{
		{"string", `"a"`}, {"number", `5`}, {"boolean", `true`}, {"array", `[]`},
	} {
		t.Run(tc.class+" item", func(t *testing.T) {
			t.Parallel()
			want := fmt.Sprintf("item %d is a JSON %s, not an object", index, tc.class)
			_, _, err := objectDecoder(accept)("/v1/x", index, json.RawMessage(tc.item))
			require.ErrorContains(t, err, want)
			_, _, err = projectDecoder("")("/v1/x", index, json.RawMessage(tc.item))
			require.ErrorContains(t, err, want)
		})
	}
	_, _, err := projectDecoder("")("/v1/x", index, json.RawMessage(`{"id":5}`))
	require.ErrorContains(t, err, fmt.Sprintf("item %d has a field of the wrong type: ", index))
	require.ErrorContains(t, err, "cannot unmarshal number")
}
