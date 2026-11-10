package catalog

import (
	"errors"
	"net/http"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const oneChartIndex = `{"apiVersion":"v1","entries":{"nginx":[{"name":"nginx","version":"1.2.3","appVersion":"4.5","description":"web","annotations":{"catalog.cattle.io/hidden":"true"},"digest":"sha256:abc","urls":["u"]}]}}`

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
			assert.Equal(t, "sha256:abc", v.Digest)
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

const infoAnswer = `{"values":{"replicas":2,"image":{"tag":"v1"}},"questions":null,"chart":{"name":"a b","version":"1.0.0+build","annotations":{"catalog.cattle.io/namespace":"cattle-x"}},"readme":"r","appReadme":"a"}`

// TestReadInfoPath asserts the request path: the local cluster has no cluster
// prefix, another cluster is reached through its proxy, and the chart name
// and version are escaped as query values.
func TestReadInfoPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, cluster, want string
	}{
		{"local", "local", "/v1/catalog.cattle.io.clusterrepos/r1?link=info&chartName=a+b&version=1.0.0%2Bbuild"},
		{"downstream", "c-1", "/k8s/clusters/c-1/v1/catalog.cattle.io.clusterrepos/r1?link=info&chartName=a+b&version=1.0.0%2Bbuild"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := newFakeRoot(map[string]rootAnswer{tc.want: {status: http.StatusOK, body: infoAnswer}})
			_, err := ReadInfo(t.Context(), root, tc.cluster, "r1", "a b", "1.0.0+build")
			require.NoError(t, err)
			assert.Equal(t, []string{tc.want}, root.requests())
		})
	}
}

// TestReadInfoDecodes asserts the default values and the chart annotations
// decode.
func TestReadInfoDecodes(t *testing.T) {
	t.Parallel()
	const path = "/v1/catalog.cattle.io.clusterrepos/r1?link=info&chartName=nginx&version=1.2.3"
	root := newFakeRoot(map[string]rootAnswer{path: {status: http.StatusOK, body: infoAnswer}})
	info, err := ReadInfo(t.Context(), root, "local", "r1", "nginx", "1.2.3")
	require.NoError(t, err)
	assert.EqualValues(t, 2, info.Values["replicas"])
	assert.Equal(t, map[string]any{"tag": "v1"}, info.Values["image"])
	assert.Equal(t, map[string]string{"catalog.cattle.io/namespace": "cattle-x"}, info.Chart.Annotations)
}

// TestReadInfoErrorsNameTheRepository asserts a failed info read keeps the
// status table's code, except 401 which is GENERIC as for the index, and
// names the repository.
func TestReadInfoErrorsNameTheRepository(t *testing.T) {
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
				"/v1/catalog.cattle.io.clusterrepos/r1?link=info&chartName=nginx&version=1.2.3": {status: tc.status},
			})
			_, err := ReadInfo(t.Context(), root, "local", "r1", "nginx", "1.2.3")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, tc.code, ce.Code)
			require.ErrorContains(t, err, "ClusterRepo 'r1'")
		})
	}
}

// TestReadInfoNotJSON asserts an info answer that does not decode is GENERIC,
// names the repository, chart and version, and never quotes the body.
func TestReadInfoNotJSON(t *testing.T) {
	t.Parallel()
	root := newFakeRoot(map[string]rootAnswer{
		"/v1/catalog.cattle.io.clusterrepos/r1?link=info&chartName=nginx&version=1.2.3": {status: http.StatusOK, body: "<html>secret-body-text</html>"},
	})
	_, err := ReadInfo(t.Context(), root, "local", "r1", "nginx", "1.2.3")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	require.ErrorContains(t, err, "'nginx'")
	require.ErrorContains(t, err, "'1.2.3'")
	require.ErrorContains(t, err, "ClusterRepo 'r1'")
	assert.NotContains(t, err.Error(), "secret-body-text")
}
