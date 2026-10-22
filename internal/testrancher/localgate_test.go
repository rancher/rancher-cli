package testrancher_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	gateAdmin  = "admin:secret"
	gateMember = "member:secret"
)

func gatedFacade(t *testing.T) (*recordingHandler, *httptest.Server) {
	t.Helper()
	fall := &recordingHandler{}
	_, srv := newFacade(t, testrancher.Options{
		LocalManagementGate: func(bearer string) bool { return bearer == gateAdmin },
		ClusterVisibility: func(bearer string) ([]testrancher.ClusterFixture, bool) {
			switch bearer {
			case gateAdmin:
				return []testrancher.ClusterFixture{{ID: "local", DisplayName: "local"}, {ID: "c-abc", DisplayName: "prod"}}, true
			case gateMember:
				return []testrancher.ClusterFixture{{ID: "c-abc", DisplayName: "prod"}}, true
			}
			return nil, false
		},
	}, fall)
	return fall, srv
}

// TestLocalManagementGateRefusesAMemberBodyless asserts the gate answers a
// bearer that may not get cluster local 403 with no body and no
// Content-Type for a management resource outside the allow list, in the
// cluster-wide and the namespaced form, and lets the admin through.
func TestLocalManagementGateRefusesAMemberBodyless(t *testing.T) {
	t.Parallel()
	fall, srv := gatedFacade(t)
	for _, path := range []string{
		"/k8s/clusters/local/apis/management.cattle.io/v3/clusters",
		"/k8s/clusters/local/apis/management.cattle.io/v3/clusters/c-abc",
		"/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-abc/clusterregistrationtokens",
	} {
		resp, body := doJSON(t, srv.Client(), http.MethodGet, srv.URL+path, nil, gateMember)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, path)
		assert.Empty(t, body, path)
		assert.Empty(t, resp.Header.Get("Content-Type"), path)
	}
	assert.Empty(t, fall.requests, "a refused request never reaches the fallthrough")

	resp, _ := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/k8s/clusters/local/apis/management.cattle.io/v3/clusters", nil, gateAdmin)
	assert.Equal(t, http.StatusTeapot, resp.StatusCode, "the admin reaches the fallthrough")
}

// TestLocalManagementGateLetsAllowListedResourcesThrough asserts the
// resources Rancher serves to every signed-in user, the discovery
// documents and other clusters' paths are not gated.
func TestLocalManagementGateLetsAllowListedResourcesThrough(t *testing.T) {
	t.Parallel()
	fall, srv := gatedFacade(t)
	for _, path := range []string{
		"/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-abc/projects",
		"/k8s/clusters/local/apis/management.cattle.io/v3/users/u-1",
		"/k8s/clusters/local/apis/management.cattle.io/v3",
		"/k8s/clusters/local/api/v1/namespaces",
		"/k8s/clusters/c-abc/apis/management.cattle.io/v3/clusters",
	} {
		resp, _ := doJSON(t, srv.Client(), http.MethodGet, srv.URL+path, nil, gateMember)
		assert.Equal(t, http.StatusTeapot, resp.StatusCode, path)
	}
	assert.Len(t, fall.requests, 5)
}

// TestSteveClusterGetByID asserts the by-id route serves a cluster the
// bearer sees with the metadata.state steve computes, answers 403 for one it
// does not, and 401 for an unknown bearer.
func TestSteveClusterGetByID(t *testing.T) {
	t.Parallel()
	_, srv := gatedFacade(t)
	status, body := getDecoded(t, srv, testrancher.ClustersCollectionPath+"/c-abc", gateMember)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "c-abc", body["id"])
	assert.Equal(t, map[string]any{"displayName": "prod"}, body["spec"])
	assert.Equal(t, "active", stateName(t, body))

	status, _ = getDecoded(t, srv, testrancher.ClustersCollectionPath+"/local", gateMember)
	assert.Equal(t, http.StatusForbidden, status)

	status, _ = getDecoded(t, srv, testrancher.ClustersCollectionPath+"/local", gateAdmin)
	assert.Equal(t, http.StatusOK, status)

	status, _ = getDecoded(t, srv, testrancher.ClustersCollectionPath+"/c-abc", "nobody:secret")
	assert.Equal(t, http.StatusUnauthorized, status)
}

// stateName returns metadata.state.name of a decoded steve object.
func stateName(t *testing.T, obj map[string]any) string {
	t.Helper()
	meta, ok := obj["metadata"].(map[string]any)
	require.True(t, ok, "metadata is an object: %v", obj)
	state, ok := meta["state"].(map[string]any)
	require.True(t, ok, "metadata.state is an object: %v", meta)
	name, _ := state["name"].(string)
	return name
}

// TestSteveClusterGetByIDOfAStoredObjectCarriesState asserts the by-id route
// adds metadata.state to the stored cluster the harness serves when it has
// none, and keeps the one it has.
func TestSteveClusterGetByIDOfAStoredObjectCarriesState(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{
		ClusterVisibility: func(string) ([]testrancher.ClusterFixture, bool) {
			return []testrancher.ClusterFixture{{ID: "c-abc", DisplayName: "prod"}, {ID: "c-def", DisplayName: "stage"}}, true
		},
		ClusterObject: func(id string) (map[string]any, bool) {
			meta := map[string]any{"name": id}
			if id == "c-def" {
				meta["state"] = map[string]any{"name": "provisioning"}
			}
			return map[string]any{"metadata": meta, "spec": map[string]any{"displayName": id}}, true
		},
	}, &recordingHandler{})
	status, body := getDecoded(t, srv, testrancher.ClustersCollectionPath+"/c-abc", gateAdmin)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "active", stateName(t, body))

	status, body = getDecoded(t, srv, testrancher.ClustersCollectionPath+"/c-def", gateAdmin)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "provisioning", stateName(t, body), "a state the object has stays")
}

// TestSteveClustersCollectionItemsCarryState asserts every item of the steve
// clusters collection carries metadata.state, as steve serves it.
func TestSteveClustersCollectionItemsCarryState(t *testing.T) {
	t.Parallel()
	_, srv := gatedFacade(t)
	status, body := getDecoded(t, srv, testrancher.ClustersCollectionPath, gateAdmin)
	require.Equal(t, http.StatusOK, status)
	items := projectItems(t, body)
	require.Len(t, items, 2)
	for _, item := range items {
		assert.Equal(t, "active", stateName(t, item), "%v", item["id"])
	}
}
