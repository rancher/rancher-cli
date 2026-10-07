package testrancher_test

import (
	"net/http"
	"testing"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	gateAdmin  = "admin:secret"
	gateMember = "member:secret"
)

func gatedFacade(t *testing.T) (*recordingHandler, string) {
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
	return fall, srv.URL
}

// TestLocalManagementGateRefusesAMemberBodyless asserts the gate answers a
// bearer that may not get cluster local 403 with no body and no
// Content-Type for a management resource outside the allow list, in the
// cluster-wide and the namespaced form, and lets the admin through.
func TestLocalManagementGateRefusesAMemberBodyless(t *testing.T) {
	t.Parallel()
	fall, url := gatedFacade(t)
	for _, path := range []string{
		"/k8s/clusters/local/apis/management.cattle.io/v3/clusters",
		"/k8s/clusters/local/apis/management.cattle.io/v3/clusters/c-abc",
		"/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-abc/clusterregistrationtokens",
	} {
		resp, body := doJSON(t, http.DefaultClient, http.MethodGet, url+path, nil, gateMember)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, path)
		assert.Empty(t, body, path)
		assert.Empty(t, resp.Header.Get("Content-Type"), path)
	}
	assert.Empty(t, fall.requests, "a refused request never reaches the fallthrough")

	resp, _ := doJSON(t, http.DefaultClient, http.MethodGet, url+"/k8s/clusters/local/apis/management.cattle.io/v3/clusters", nil, gateAdmin)
	assert.Equal(t, http.StatusTeapot, resp.StatusCode, "the admin reaches the fallthrough")
}

// TestLocalManagementGateLetsAllowListedResourcesThrough asserts the
// resources Rancher serves to every signed-in user, the discovery
// documents and other clusters' paths are not gated.
func TestLocalManagementGateLetsAllowListedResourcesThrough(t *testing.T) {
	t.Parallel()
	fall, url := gatedFacade(t)
	for _, path := range []string{
		"/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-abc/projects",
		"/k8s/clusters/local/apis/management.cattle.io/v3/users/u-1",
		"/k8s/clusters/local/apis/management.cattle.io/v3",
		"/k8s/clusters/local/api/v1/namespaces",
		"/k8s/clusters/c-abc/apis/management.cattle.io/v3/clusters",
	} {
		resp, _ := doJSON(t, http.DefaultClient, http.MethodGet, url+path, nil, gateMember)
		assert.Equal(t, http.StatusTeapot, resp.StatusCode, path)
	}
	assert.Len(t, fall.requests, 5)
}

// TestSteveClusterGetByID asserts the by-id route serves a cluster the
// bearer sees, answers 403 for one it does not, and 401 for an unknown
// bearer.
func TestSteveClusterGetByID(t *testing.T) {
	t.Parallel()
	_, url := gatedFacade(t)
	status, body := getDecoded(t, url, testrancher.ClustersCollectionPath+"/c-abc", gateMember)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "c-abc", body["id"])
	assert.Equal(t, map[string]any{"displayName": "prod"}, body["spec"])

	status, _ = getDecoded(t, url, testrancher.ClustersCollectionPath+"/local", gateMember)
	assert.Equal(t, http.StatusForbidden, status)

	status, _ = getDecoded(t, url, testrancher.ClustersCollectionPath+"/local", gateAdmin)
	assert.Equal(t, http.StatusOK, status)

	status, _ = getDecoded(t, url, testrancher.ClustersCollectionPath+"/c-abc", "nobody:secret")
	assert.Equal(t, http.StatusUnauthorized, status)
}
