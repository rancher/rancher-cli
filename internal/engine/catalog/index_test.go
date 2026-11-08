package catalog

import (
	"errors"
	"net/http"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const oneChartIndex = `{"apiVersion":"v1","entries":{"nginx":[{"name":"nginx","version":"1.2.3","appVersion":"4.5","description":"web","annotations":{"catalog.cattle.io/hidden":"true"},"digest":"x","urls":["u"]}]}}`

// TestReadIndexPath asserts the request path: the local cluster has no
// cluster prefix, another cluster is reached through its proxy, and the
// repository name is escaped as one path segment.
func TestReadIndexPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, cluster, repo, want string
	}{
		{"local", "local", "rancher-charts", "/v1/catalog.cattle.io.clusterrepos/rancher-charts?link=index"},
		{"downstream", "c-abc", "rancher-charts", "/k8s/clusters/c-abc/v1/catalog.cattle.io.clusterrepos/rancher-charts?link=index"},
		{"name that needs escaping", "c-abc", "my repo/x?y", "/k8s/clusters/c-abc/v1/catalog.cattle.io.clusterrepos/my%20repo%2Fx%3Fy?link=index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := newFakeRoot(map[string]rootAnswer{tc.want: {status: http.StatusOK, body: oneChartIndex}})
			idx, err := ReadIndex(t.Context(), root, tc.cluster, tc.repo)
			require.NoError(t, err)
			assert.Equal(t, []string{tc.want}, root.requests())
			require.Contains(t, idx.Entries, "nginx")
			v := idx.Entries["nginx"][0]
			assert.Equal(t, "1.2.3", v.Version)
			assert.Equal(t, "4.5", v.AppVersion)
			assert.Equal(t, "web", v.Description)
			assert.Equal(t, "true", v.Annotations["catalog.cattle.io/hidden"])
		})
	}
}

// TestReadIndexNotJSON asserts an index that does not decode is GENERIC,
// names the repository and never quotes the body.
func TestReadIndexNotJSON(t *testing.T) {
	t.Parallel()
	const body = "<html>secret-body-text</html>"
	root := newFakeRoot(map[string]rootAnswer{
		"/v1/catalog.cattle.io.clusterrepos/r1?link=index": {status: http.StatusOK, body: body},
	})
	_, err := ReadIndex(t.Context(), root, "local", "r1")
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	require.ErrorContains(t, err, "'r1'")
	assert.NotContains(t, err.Error(), "secret-body-text")
}

// TestReadIndexErrorsNameTheRepository asserts every failed read keeps the
// status table's code and names the repository.
func TestReadIndexErrorsNameTheRepository(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"forbidden", http.StatusForbidden, clierr.CodeRBAC},
		{"not found", http.StatusNotFound, clierr.CodeNotFound},
		{"unauthorized", http.StatusUnauthorized, clierr.CodeGeneric},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := newFakeRoot(map[string]rootAnswer{
				"/v1/catalog.cattle.io.clusterrepos/r1?link=index": {status: tc.status},
			})
			_, err := ReadIndex(t.Context(), root, "local", "r1")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, tc.code, ce.Code)
			require.ErrorContains(t, err, "ClusterRepo 'r1'")
		})
	}
}

// TestReadIndex401IsNotARejectedCredential asserts a 401 from the index read,
// which Rancher also answers for an index it will not serve, names the
// repository, says Rancher refused to serve its index, and carries no
// sign-in hint.
func TestReadIndex401IsNotARejectedCredential(t *testing.T) {
	t.Parallel()
	root := newFakeRoot(map[string]rootAnswer{
		"/v1/catalog.cattle.io.clusterrepos/r1?link=index": {status: http.StatusUnauthorized},
	})
	_, err := ReadIndex(t.Context(), root, "local", "r1")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Empty(t, ce.Hint)
	assert.False(t, ce.Retryable)
	require.ErrorContains(t, err, "ClusterRepo 'r1': Rancher refused to serve its index")
	assert.NotContains(t, err.Error(), "credential")
	assert.NotContains(t, err.Error(), "auth status")
}
