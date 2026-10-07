package kubeconfig

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
)

// TestDirectEndpointsOffIsSilent asserts a cluster without the endpoint
// yields no entries and no reasons.
func TestDirectEndpointsOffIsSilent(t *testing.T) {
	t.Parallel()
	for name, obj := range map[string]*unstructured.Unstructured{
		"disabled": mgmtCluster("c-1", false, "", "", ""),
		"absent":   {Object: map[string]any{"metadata": map[string]any{"name": "c-1"}, "spec": map[string]any{}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, err := DirectEndpoints(t.Context(), &fakeClient{}, newClusterGetter(obj).Get, "c-1")
			require.NoError(t, err)
			assert.Empty(t, d.Endpoints)
			assert.Empty(t, d.Omitted)
		})
	}
}

// TestDirectEndpointsFQDN asserts the fqdn entry with and without caCerts,
// and that an unparsable caCerts is an omission.
func TestDirectEndpointsFQDN(t *testing.T) {
	t.Parallel()
	pem := testPEM(t)
	t.Run("with ca", func(t *testing.T) {
		t.Parallel()
		c := &fakeClient{Objects: map[string]*unstructured.Unstructured{mcmFeature: feature(true)}}
		get := newClusterGetter(mgmtCluster("c-1", true, "ace.example.test", string(pem), "")).Get
		d, err := DirectEndpoints(t.Context(), c, get, "c-1")
		require.NoError(t, err)
		require.Len(t, d.Endpoints, 1)
		assert.Equal(t, Endpoint{Name: "fqdn", Server: "https://ace.example.test", CAData: pem, Ready: true}, d.Endpoints[0])
		assert.Empty(t, d.Omitted)
	})
	t.Run("without ca", func(t *testing.T) {
		t.Parallel()
		c := &fakeClient{Objects: map[string]*unstructured.Unstructured{mcmFeature: feature(true)}}
		get := newClusterGetter(mgmtCluster("c-1", true, "ace.example.test", "", "")).Get
		d, err := DirectEndpoints(t.Context(), c, get, "c-1")
		require.NoError(t, err)
		require.Len(t, d.Endpoints, 1)
		assert.Nil(t, d.Endpoints[0].CAData)
	})
	t.Run("bad ca", func(t *testing.T) {
		t.Parallel()
		c := &fakeClient{Objects: map[string]*unstructured.Unstructured{mcmFeature: feature(true)}}
		get := newClusterGetter(mgmtCluster("c-1", true, "ace.example.test", "not a pem", "")).Get
		d, err := DirectEndpoints(t.Context(), c, get, "c-1")
		require.NoError(t, err)
		assert.Empty(t, d.Endpoints)
		require.Len(t, d.Omitted, 1)
		assert.Contains(t, d.Omitted[0], "caCerts is not a PEM")
	})
	t.Run("fqdn with a path", func(t *testing.T) {
		t.Parallel()
		c := &fakeClient{Objects: map[string]*unstructured.Unstructured{mcmFeature: feature(true)}}
		get := newClusterGetter(mgmtCluster("c-1", true, "ace.example.test/k8s/clusters/local", "", "")).Get
		d, err := DirectEndpoints(t.Context(), c, get, "c-1")
		require.NoError(t, err)
		assert.Empty(t, d.Endpoints)
		require.Len(t, d.Omitted, 1)
		assert.Contains(t, d.Omitted[0], "path in")
	})
}

// TestDirectEndpointsNodes asserts the per-node entries: the address
// precedence (ExternalIP, the status annotation gated on metadata
// annotations, the last InternalIP), IPv6 bracketing, the worker skipped,
// the decoded status CA, and Ready read from the condition.
func TestDirectEndpointsNodes(t *testing.T) {
	t.Parallel()
	pem := testPEM(t)
	nodes := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		mgmtNode("m-a", "c-1-cp1", true, true, true, "", [2]string{"InternalIP", "10.0.0.1"}, [2]string{"ExternalIP", "203.0.113.1"}),
		mgmtNode("m-b", "c-1-cp2", true, false, true, "198.51.100.2", [2]string{"InternalIP", "10.0.0.2"}),
		mgmtNode("m-c", "c-1-cp3", true, true, false, "198.51.100.3", [2]string{"InternalIP", "10.0.0.3"}, [2]string{"InternalIP", "10.0.0.33"}),
		mgmtNode("m-d", "c-1-cp6", true, true, true, "", [2]string{"InternalIP", "fd00::1"}),
		mgmtNode("m-w", "c-1-w1", false, true, true, "", [2]string{"InternalIP", "10.0.0.9"}),
	}}
	c := &fakeClient{
		Objects: map[string]*unstructured.Unstructured{mcmFeature: feature(true)},
		Lists:   map[string]*unstructured.UnstructuredList{"nodes|c-1": nodes},
	}
	get := newClusterGetter(mgmtCluster("c-1", true, "", "", b64(pem))).Get
	d, err := DirectEndpoints(t.Context(), c, get, "c-1")
	require.NoError(t, err)
	assert.Empty(t, d.Omitted)
	require.Len(t, d.Endpoints, 4)
	assert.Equal(t, Endpoint{Name: "cp1", Server: "https://203.0.113.1:6443", CAData: pem, Ready: true}, d.Endpoints[0], "ExternalIP wins")
	assert.Equal(t, Endpoint{Name: "cp2", Server: "https://198.51.100.2:6443", CAData: pem, Ready: false}, d.Endpoints[1], "the status annotation when metadata annotations exist")
	assert.Equal(t, Endpoint{Name: "cp3", Server: "https://10.0.0.33:6443", CAData: pem, Ready: true}, d.Endpoints[2], "no metadata annotations: the annotation is ignored, the last InternalIP wins")
	assert.Equal(t, Endpoint{Name: "cp6", Server: "https://[fd00::1]:6443", CAData: pem, Ready: true}, d.Endpoints[3], "IPv6 is bracketed")
}

// TestDirectEndpointsNodeDefects asserts each defective node is skipped and
// named while the others are emitted, and that zero control-plane nodes,
// a bad status CA, and a refused node list are omissions.
func TestDirectEndpointsNodeDefects(t *testing.T) {
	t.Parallel()
	pem := testPEM(t)
	base := func() *fakeClient {
		return &fakeClient{Objects: map[string]*unstructured.Unstructured{mcmFeature: feature(true)}}
	}
	get := newClusterGetter(mgmtCluster("c-1", true, "", "", b64(pem))).Get
	t.Run("skipped nodes", func(t *testing.T) {
		t.Parallel()
		c := base()
		c.Lists = map[string]*unstructured.UnstructuredList{"nodes|c-1": {Items: []unstructured.Unstructured{
			mgmtNode("m-a", "c-1-cp1", true, true, true, "", [2]string{"InternalIP", "10.0.0.1"}),
			mgmtNode("m-b", "", true, true, true, "", [2]string{"InternalIP", "10.0.0.2"}),
			mgmtNode("m-c", "c-1-cp1", true, true, true, "", [2]string{"InternalIP", "10.0.0.3"}),
			mgmtNode("m-d", "c-1-cp4", true, true, true, ""),
		}}}
		d, err := DirectEndpoints(t.Context(), c, get, "c-1")
		require.NoError(t, err)
		require.Len(t, d.Endpoints, 1)
		assert.Equal(t, "cp1", d.Endpoints[0].Name)
		require.Len(t, d.Omitted, 3)
		assert.Contains(t, d.Omitted[0], "m-b: empty requestedHostname")
		assert.Contains(t, d.Omitted[1], "m-c: duplicate context name cp1")
		assert.Contains(t, d.Omitted[2], "m-d: no address")
	})
	t.Run("no control-plane node", func(t *testing.T) {
		t.Parallel()
		c := base()
		c.Lists = map[string]*unstructured.UnstructuredList{"nodes|c-1": {Items: []unstructured.Unstructured{mgmtNode("m-w", "c-1-w1", false, true, true, "", [2]string{"InternalIP", "10.0.0.9"})}}}
		d, err := DirectEndpoints(t.Context(), c, get, "c-1")
		require.NoError(t, err)
		assert.Empty(t, d.Endpoints)
		require.Len(t, d.Omitted, 1)
		assert.Contains(t, d.Omitted[0], "no control-plane node")
	})
	t.Run("bad status ca", func(t *testing.T) {
		t.Parallel()
		c := &fakeClient{Objects: map[string]*unstructured.Unstructured{mcmFeature: feature(true)}}
		get := newClusterGetter(mgmtCluster("c-1", true, "", "", "%%%not-base64")).Get
		d, err := DirectEndpoints(t.Context(), c, get, "c-1")
		require.NoError(t, err)
		assert.Empty(t, d.Endpoints)
		require.Len(t, d.Omitted, 1)
		assert.Contains(t, d.Omitted[0], "status.caCert is not a base64 PEM")
	})
	t.Run("node list refused", func(t *testing.T) {
		t.Parallel()
		c := base()
		c.ListErr = clierr.New(clierr.CodeRBAC, "nodes is forbidden")
		d, err := DirectEndpoints(t.Context(), c, get, "c-1")
		require.NoError(t, err)
		assert.Empty(t, d.Endpoints)
		require.Len(t, d.Omitted, 1)
		assert.Contains(t, d.Omitted[0], "not readable")
	})
}

// TestDirectEndpointsMalformedNodeFields asserts a present node field of
// the wrong shape skips that node and names it, and never emits a context
// picked past the defect: a scalar address entry, a non-string address
// field, a non-string-map nodeAnnotations, a non-object condition, and a
// non-string condition status.
func TestDirectEndpointsMalformedNodeFields(t *testing.T) {
	t.Parallel()
	pem := testPEM(t)
	good := mgmtNode("m-ok", "c-1-ok", true, true, true, "", [2]string{"InternalIP", "10.0.0.1"})
	scalarAddress := mgmtNode("m-a", "c-1-a", true, true, true, "", [2]string{"InternalIP", "10.0.0.2"})
	scalarAddress.Object["status"].(map[string]any)["internalNodeStatus"].(map[string]any)["addresses"] = []any{"10.0.0.2"}
	badAddressField := mgmtNode("m-b", "c-1-b", true, true, true, "", [2]string{"InternalIP", "10.0.0.3"})
	badAddressField.Object["status"].(map[string]any)["internalNodeStatus"].(map[string]any)["addresses"] = []any{map[string]any{"type": "InternalIP", "address": int64(3)}}
	badAnnotations := mgmtNode("m-c", "c-1-c", true, true, true, "", [2]string{"InternalIP", "10.0.0.4"})
	badAnnotations.Object["status"].(map[string]any)["nodeAnnotations"] = "not a map"
	badCondition := mgmtNode("m-d", "c-1-d", true, true, true, "", [2]string{"InternalIP", "10.0.0.5"})
	badCondition.Object["status"].(map[string]any)["internalNodeStatus"].(map[string]any)["conditions"] = []any{"Ready"}
	badStatus := mgmtNode("m-e", "c-1-e", true, true, true, "", [2]string{"InternalIP", "10.0.0.6"})
	badStatus.Object["status"].(map[string]any)["internalNodeStatus"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": true}}
	// Defects past an otherwise usable value: an ExternalIP followed by a
	// scalar entry, a Ready condition followed by a scalar entry, and a
	// metadata annotation whose value is not a string.
	lateAddress := mgmtNode("m-f", "c-1-f", true, true, true, "", [2]string{"ExternalIP", "203.0.113.6"})
	lateAddress.Object["status"].(map[string]any)["internalNodeStatus"].(map[string]any)["addresses"] = []any{map[string]any{"type": "ExternalIP", "address": "203.0.113.6"}, "garbage"}
	lateCondition := mgmtNode("m-g", "c-1-g", true, true, true, "", [2]string{"InternalIP", "10.0.0.7"})
	lateCondition.Object["status"].(map[string]any)["internalNodeStatus"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": "True"}, "garbage"}
	badMetaAnnotations := mgmtNode("m-h", "c-1-h", true, true, true, "", [2]string{"InternalIP", "10.0.0.8"})
	badMetaAnnotations.Object["metadata"].(map[string]any)["annotations"] = map[string]any{"a": int64(1)}
	c := &fakeClient{
		Objects: map[string]*unstructured.Unstructured{mcmFeature: feature(true)},
		Lists:   map[string]*unstructured.UnstructuredList{"nodes|c-1": {Items: []unstructured.Unstructured{good, scalarAddress, badAddressField, badAnnotations, badCondition, badStatus, lateAddress, lateCondition, badMetaAnnotations}}},
	}
	get := newClusterGetter(mgmtCluster("c-1", true, "", "", b64(pem))).Get
	d, err := DirectEndpoints(t.Context(), c, get, "c-1")
	require.NoError(t, err)
	require.Len(t, d.Endpoints, 1)
	assert.Equal(t, "ok", d.Endpoints[0].Name)
	require.Len(t, d.Omitted, 8)
	assert.Contains(t, d.Omitted[0], "m-a: status.internalNodeStatus.addresses[0] is not an object")
	assert.Contains(t, d.Omitted[1], "m-b: status.internalNodeStatus.addresses[0]: address is not a string")
	assert.Contains(t, d.Omitted[2], "m-c: status.nodeAnnotations is not a string map")
	assert.Contains(t, d.Omitted[3], "m-d: status.internalNodeStatus.conditions[0] is not an object")
	assert.Contains(t, d.Omitted[4], "m-e: status.internalNodeStatus.conditions[0]: status is not a string")
	assert.Contains(t, d.Omitted[5], "m-f: status.internalNodeStatus.addresses[1] is not an object", "a defect after a usable ExternalIP still skips the node")
	assert.Contains(t, d.Omitted[6], "m-g: status.internalNodeStatus.conditions[1] is not an object", "a defect after the Ready condition still skips the node")
	assert.Contains(t, d.Omitted[7], "m-h: metadata.annotations is not a string map")
}

// TestDirectEndpointsOrderFollowsNodeName asserts the entries, and so the
// current-context choice, follow the node name whatever order the list
// arrived in: Rancher's generator walks an informer cache with no defined
// order, and two runs against one cluster must agree.
func TestDirectEndpointsOrderFollowsNodeName(t *testing.T) {
	t.Parallel()
	pem := testPEM(t)
	c := &fakeClient{
		Objects: map[string]*unstructured.Unstructured{mcmFeature: feature(true)},
		Lists: map[string]*unstructured.UnstructuredList{"nodes|c-1": {Items: []unstructured.Unstructured{
			mgmtNode("m-b", "c-1-cp2", true, true, true, "", [2]string{"InternalIP", "10.0.0.2"}),
			mgmtNode("m-a", "c-1-cp1", true, true, true, "", [2]string{"InternalIP", "10.0.0.1"}),
		}}},
	}
	get := newClusterGetter(mgmtCluster("c-1", true, "", "", b64(pem))).Get
	d, err := DirectEndpoints(t.Context(), c, get, "c-1")
	require.NoError(t, err)
	require.Len(t, d.Endpoints, 2)
	assert.Equal(t, "cp1", d.Endpoints[0].Name, "the first Ready entry by node name, not by list order")
	assert.Equal(t, "cp2", d.Endpoints[1].Name)
}

// TestDirectEndpointsFeatureAndClusterReads asserts the feature gate and
// the management cluster read: off, unreadable, and a refused cluster GET
// are omissions; a missing cluster is the error; a cancelled context is
// returned as its own error from every read.
func TestDirectEndpointsFeatureAndClusterReads(t *testing.T) {
	t.Parallel()
	pem := testPEM(t)
	t.Run("feature off", func(t *testing.T) {
		t.Parallel()
		c := &fakeClient{Objects: map[string]*unstructured.Unstructured{mcmFeature: feature(false)}}
		get := newClusterGetter(mgmtCluster("c-1", true, "ace.example.test", string(pem), "")).Get
		d, err := DirectEndpoints(t.Context(), c, get, "c-1")
		require.NoError(t, err)
		assert.Empty(t, d.Endpoints)
		require.Len(t, d.Omitted, 1)
		assert.Contains(t, d.Omitted[0], "feature is off")
	})
	t.Run("feature unreadable", func(t *testing.T) {
		t.Parallel()
		c := &fakeClient{GetErrFor: map[string]error{mcmFeature: clierr.New(clierr.CodeRBAC, "features is forbidden")}}
		get := newClusterGetter(mgmtCluster("c-1", true, "ace.example.test", string(pem), "")).Get
		d, err := DirectEndpoints(t.Context(), c, get, "c-1")
		require.NoError(t, err)
		require.Len(t, d.Omitted, 1)
		assert.Contains(t, d.Omitted[0], "feature is not readable")
	})
	t.Run("cluster get refused", func(t *testing.T) {
		t.Parallel()
		get := newClusterGetter()
		get.errs = map[string]error{"c-1": clierr.New(clierr.CodeRBAC, "clusters is forbidden")}
		d, err := DirectEndpoints(t.Context(), &fakeClient{}, get.Get, "c-1")
		require.NoError(t, err)
		require.Len(t, d.Omitted, 1)
		assert.Contains(t, d.Omitted[0], "not readable")
	})
	t.Run("cluster not found", func(t *testing.T) {
		t.Parallel()
		_, err := DirectEndpoints(t.Context(), &fakeClient{}, newClusterGetter().Get, "c-1")
		require.Error(t, err)
		assert.True(t, clierr.IsNotFound(err))
	})
	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		get := newClusterGetter(mgmtCluster("c-1", true, "ace.example.test", "", ""))
		get.before = func(ctx context.Context) error { return ctx.Err() }
		c := &fakeClient{Objects: map[string]*unstructured.Unstructured{mcmFeature: feature(true)}}
		_, err := DirectEndpoints(ctx, c, get.Get, "c-1")
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("wrong field type", func(t *testing.T) {
		t.Parallel()
		obj := mgmtCluster("c-1", true, "", "", "")
		obj.Object["spec"].(map[string]any)["localClusterAuthEndpoint"].(map[string]any)["enabled"] = "yes"
		d, err := DirectEndpoints(t.Context(), &fakeClient{}, newClusterGetter(obj).Get, "c-1")
		require.NoError(t, err)
		require.Len(t, d.Omitted, 1)
		assert.Contains(t, d.Omitted[0], "enabled is not a bool")
	})
}

// TestDirectEndpointsReadsTheClusterOnlyThroughTheGetter asserts the
// management cluster is read once, as the steve request for that cluster,
// through the getter, and never through the management client: the client
// holds a decoy cluster that would change the answer, and records no Get of
// the clusters resource on either the fqdn or the node path.
func TestDirectEndpointsReadsTheClusterOnlyThroughTheGetter(t *testing.T) {
	t.Parallel()
	pem := testPEM(t)
	decoy := mgmtCluster("c-1", false, "", "", "")
	for name, tc := range map[string]struct {
		cluster *unstructured.Unstructured
		want    Endpoint
	}{
		"fqdn": {mgmtCluster("c-1", true, "ace.example.test", string(pem), ""), Endpoint{Name: "fqdn", Server: "https://ace.example.test", CAData: pem, Ready: true}},
		"node": {mgmtCluster("c-1", true, "", "", b64(pem)), Endpoint{Name: "cp1", Server: "https://10.0.0.1:6443", CAData: pem, Ready: true}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &fakeClient{
				Objects: map[string]*unstructured.Unstructured{"c-1": decoy, mcmFeature: feature(true)},
				Lists: map[string]*unstructured.UnstructuredList{"nodes|c-1": {Items: []unstructured.Unstructured{
					mgmtNode("m-a", "c-1-cp1", true, true, true, "", [2]string{"InternalIP", "10.0.0.1"}),
				}}},
			}
			get := newClusterGetter(tc.cluster)
			d, err := DirectEndpoints(t.Context(), c, get.Get, "c-1")
			require.NoError(t, err)
			assert.Empty(t, d.Omitted)
			assert.Equal(t, []Endpoint{tc.want}, d.Endpoints)
			assert.Equal(t, []steve.Request{{Type: steve.ClustersType, Name: "c-1"}}, get.requests)
			assert.NotContains(t, c.Gets, "clusters")
			assert.Equal(t, []string{"features"}, c.Gets, "the Feature is still read through the management client")
		})
	}
}

// TestDirectEndpointsForbiddenClusterIsOmitted asserts a 403 from the
// getter lands in Omitted naming the cluster and the refused request, reads
// nothing through the management client, and returns no error: first with a
// getter that fails with a *steve.StatusError, then through the root client
// getter with the root client's own 403 error.
func TestDirectEndpointsForbiddenClusterIsOmitted(t *testing.T) {
	t.Parallel()
	forbiddenRoot := clierr.New(clierr.CodeAuth, "forbidden").WithCause(&engine.RootStatusError{Status: 403})
	for name, get := range map[string]steve.Getter{
		"status error": func(context.Context, steve.Request) ([]byte, error) { return nil, &steve.StatusError{Status: 403} },
		"root client":  steve.RootGetter(failingRoot{err: forbiddenRoot}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &fakeClient{}
			d, err := DirectEndpoints(t.Context(), c, get, "c-1")
			require.NoError(t, err)
			assert.Empty(t, d.Endpoints)
			require.Len(t, d.Omitted, 1)
			assert.Contains(t, d.Omitted[0], "management cluster c-1 not readable")
			assert.Contains(t, d.Omitted[0], "you cannot read cluster 'c-1'")
			assert.Contains(t, d.Omitted[0], "/v1/management.cattle.io.clusters/c-1 answered 403")
			assert.Empty(t, c.Gets)
		})
	}
}

// TestDirectEndpointsRootNotFoundIsAnError asserts a 404 through the root
// client getter is returned as a not-found error, not an omission.
func TestDirectEndpointsRootNotFoundIsAnError(t *testing.T) {
	t.Parallel()
	notFound := clierr.New(clierr.CodeNotFound, "not found").WithCause(&engine.RootStatusError{Status: 404})
	d, err := DirectEndpoints(t.Context(), &fakeClient{}, steve.RootGetter(failingRoot{err: notFound}), "c-1")
	require.Error(t, err)
	assert.True(t, clierr.IsNotFound(err))
	assert.Empty(t, d.Omitted)
}

// TestValidEndpointURL asserts the accepted and rejected shapes.
func TestValidEndpointURL(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"https://ace.example.test", "https://ace.example.test/", "https://[fd00::1]:6443", "https://10.0.0.1:6443"} {
		require.NoError(t, validEndpointURL(ok), ok)
	}
	for _, bad := range []string{"http://ace.example.test", "https://", "https://u:p@ace.example.test", "https://ace.example.test?x=1", "https://ace.example.test#f", "https://ace.example.test/k8s"} {
		require.Error(t, validEndpointURL(bad), bad)
	}
}
