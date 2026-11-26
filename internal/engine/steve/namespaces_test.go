package steve

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	namespacesPath = "/k8s/clusters/c-m-abc/v1/namespaces"
	namespacePath  = namespacesPath + "/team-a"
)

func steveNamespaceItem(name string) map[string]any {
	return map[string]any{"id": name, "type": "namespace", "metadata": map[string]any{"name": name}}
}

// TestNamespaceObjectsReadsEveryPageOfTheCluster asserts the namespaces of a
// cluster are read from the cluster's own Steve path, every page, in the order
// Steve returned them, and that the item of a namespace listed with a few
// fields is kept.
func TestNamespaceObjectsReadsEveryPageOfTheCluster(t *testing.T) {
	t.Parallel()
	fewFields := steveNamespaceItem("team-b")
	srv := newServer(t, map[string]route{namespacesPath: {pages: map[string]stevePage{
		"":     {Data: []map[string]any{steveNamespaceItem("team-a")}, Count: new(2), Continue: "next", Revision: "1"},
		"next": {Data: []map[string]any{fewFields}, Count: new(2), Revision: "1"},
	}}})
	got, err := NamespaceObjects(t.Context(), srv.getter(), "c-m-abc")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "team-a", got[0].GetName())
	assert.Equal(t, "team-b", got[1].GetName())
	assert.Equal(t, []string{namespacesPath, namespacesPath + "?continue=next"}, srv.requests())
}

// TestNamespaceObjectsReturnsAProtocolErrorForAnItemItCannotTrust asserts an
// item with no id, no metadata.name or that is not an object is a protocol
// error that names the path.
func TestNamespaceObjectsReturnsAProtocolErrorForAnItemItCannotTrust(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ item, want string }{
		"no id":         {`{"metadata":{"name":"team-a"}}`, fmt.Sprintf("item %d has no id", 0)},
		"no name":       {`{"id":"team-a","metadata":{}}`, `item "team-a" has no metadata.name`},
		"not an object": {`[]`, fmt.Sprintf("item %d is a JSON array, not an object", 0)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"data":[%s],"count":1,"revision":"1"}`, tc.item)
			})
			_, err := NamespaceObjects(t.Context(), srv.getter(), "c-m-abc")
			require.ErrorContains(t, err, "unexpected response from "+namespacesPath)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// TestNamespaceReadersKeepTheStatusOfARefusal asserts a 403 and a 404 of the
// namespace collection and of one namespace keep their status for
// clierr.HTTPStatus.
func TestNamespaceReadersKeepTheStatusOfARefusal(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		srv := newServer(t, map[string]route{namespacesPath: {status: status}, namespacePath: {status: status}})
		_, err := NamespaceObjects(t.Context(), srv.getter(), "c-m-abc")
		got, ok := clierr.HTTPStatus(err)
		assert.True(t, ok, "the collection, status %d", status)
		assert.Equal(t, status, got)
		_, err = GetNamespaceObject(t.Context(), srv.getter(), "c-m-abc", "team-a")
		got, ok = clierr.HTTPStatus(err)
		assert.True(t, ok, "one namespace, status %d", status)
		assert.Equal(t, status, got)
	}
}

// TestGetNamespaceObjectReadsOneNamespaceAndRefusesAnotherObject asserts one
// namespace is read from the path of its cluster and name and returned as Steve
// returned it, and a body that is not an object or names another namespace is a
// protocol error.
func TestGetNamespaceObjectReadsOneNamespaceAndRefusesAnotherObject(t *testing.T) {
	t.Parallel()
	body := func(s string) *recordingServer {
		return newHandlerServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, s) })
	}
	srv := body(`{"id":"team-a","type":"namespace","metadata":{"name":"team-a"}}`)
	obj, err := GetNamespaceObject(t.Context(), srv.getter(), "c-m-abc", "team-a")
	require.NoError(t, err)
	assert.Equal(t, "team-a", obj.GetName())
	assert.Equal(t, "namespace", obj.Object["type"], "the object is returned as Steve returned it")
	assert.Equal(t, []string{namespacePath}, srv.requests())

	for name, tc := range map[string]struct{ body, want string }{
		"not an object": {`[]`, "not a JSON object"},
		"other name":    {`{"metadata":{"name":"team-b"}}`, `the response names "team-b", not the object requested`},
		"no name":       {`{"metadata":{}}`, `the response names "", not the object requested`},
	} {
		_, err := GetNamespaceObject(t.Context(), body(tc.body).getter(), "c-m-abc", "team-a")
		require.ErrorContains(t, err, "unexpected response from "+namespacePath, name)
		require.ErrorContains(t, err, tc.want, name)
	}
}
