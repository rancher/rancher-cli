package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestVersionDiagnosticsEndWithTheirCommand asserts the NOT_FOUND texts of
// the version lookup for an API group: the two about a kind end with the
// directory-free schema command, and the one about the ext group names the
// command that lists tokens inside the sentence, followed by a semicolon.
func TestVersionDiagnosticsEndWithTheirCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		groups map[string]string
		gvr    schema.GroupVersionResource
		want   string
	}{
		{"a group with no preferred version", map[string]string{"cluster.x-k8s.io": ""},
			schema.GroupVersionResource{Group: "cluster.x-k8s.io", Resource: "clusters"},
			`API group "cluster.x-k8s.io" has no preferred version; the server may be mid-upgrade; retry, or check API requirements with: rancher schema <kind>`},
		{"a group the server does not serve", map[string]string{"cluster.x-k8s.io": "v1"},
			schema.GroupVersionResource{Group: "other.example.io", Resource: "widgets"},
			`API group "other.example.io" not served by the cluster; the kind may need a newer server version or an optional feature; check the requirements with: rancher schema <kind>`},
		{"the ext group on an older server", map[string]string{"cluster.x-k8s.io": "v1"},
			schema.GroupVersionResource{Group: "ext.cattle.io", Resource: "tokens"},
			"this Rancher does not serve ext.cattle.io (requires Rancher >= " + config.MinRancherVersion + ", the supported minimum); token management: " +
				clierr.Rancher + " auth list-tokens; kubeconfig: the default exec-credential mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{})
			disc := &stubDiscovery{groups: tc.groups}
			c := &dynamicClient{dyn: fc, groups: disc.ServerGroups, versions: map[string]string{}}
			_, err := c.Get(t.Context(), tc.gvr, "ns", "x")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
		})
	}
}

// TestADeadlineMessageEndsWithTheCommandThatInspectsTheCluster asserts the
// message of a hung request through a downstream cluster names the likely
// cause first and ends with the command that inspects that cluster, with
// the cluster's id and no placeholder.
func TestADeadlineMessageEndsWithTheCommandThatInspectsTheCluster(t *testing.T) {
	t.Parallel()
	err := &url.Error{Op: "Get", URL: "https://rancher.example/k8s/clusters/c-m-x/api/v1/nodes", Err: context.DeadlineExceeded}
	ce, ok := errors.AsType[*clierr.Error](mapRequestErr(t.Context(), http.MethodGet, err, &clierr.Request{Cluster: "c-m-x"}))
	require.True(t, ok)
	assert.True(t, strings.HasSuffix(ce.Message, "its agent may be disconnected; check the cluster with: "+clierr.Rancher+" inspect cluster 'c-m-x'"), ce.Message)
	assert.Equal(t, clierr.Rancher+" inspect cluster 'c-m-x'", InspectClusterRemedyFor("c-m-x"))
}
