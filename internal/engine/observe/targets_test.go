package observe

import (
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func clierrCode(t *testing.T, err error) string {
	t.Helper()
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	return ce.Code
}

func TestLookup(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"server", "agent", "fleet-agent", "kube-api-auth", "webhook", "api-extension"} {
		_, ok := Lookup(name)
		assert.True(t, ok, "target %q must be in the table", name)
	}
	_, ok := Lookup("bogus")
	assert.False(t, ok)
}

func TestResolveAgentDownstream(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().withListNS(podKind.GVR, "cattle-system", podList("agent-1"))
	factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	target, _ := Lookup("agent")
	resolved, err := Resolve(t.Context(), factory, factory.getter(), target, input{s: map[string]string{engine.FlagCluster: "c-1"}})
	require.NoError(t, err)
	assert.Equal(t, "c-1", resolved.ClusterID)
	assert.Equal(t, "cattle-system", resolved.Namespace)
	require.Len(t, resolved.Pods, 1)
	assert.Equal(t, "agent-1", resolved.Pods[0].GetName())
	assert.Contains(t, downstream.listSelectors, "app=cattle-cluster-agent")
}

func TestResolveDownstreamOnlyRejectsLocal(t *testing.T) {
	t.Parallel()
	factory := &fakeFactory{local: newFakeClient()}
	target, _ := Lookup("agent")

	_, err := Resolve(t.Context(), factory, factory.getter(), target, input{s: map[string]string{engine.FlagCluster: "local"}})
	require.Error(t, err)
	assert.Equal(t, clierr.CodeValidation, clierrCode(t, err))
	assert.ErrorContains(t, err, "only runs on a downstream cluster")
}

func TestResolveLocalOnlyRejectsExplicitDownstreamCluster(t *testing.T) {
	t.Parallel()
	factory := &fakeFactory{local: newFakeClient(), resolve: map[string]string{"prod": "c-prod"}}
	target, _ := Lookup("webhook")

	_, err := Resolve(t.Context(), factory, factory.getter(), target, input{s: map[string]string{engine.FlagCluster: "prod"}})
	require.Error(t, err)
	assert.Equal(t, clierr.CodeValidation, clierrCode(t, err))
	assert.ErrorContains(t, err, "local-only target")
}

func TestResolveLocalOnlyAllowsExplicitLocal(t *testing.T) {
	t.Parallel()
	local := newFakeClient().withListNS(podKind.GVR, "cattle-system", podList("webhook-1"))
	factory := &fakeFactory{local: local}
	target, _ := Lookup("webhook")

	resolved, err := Resolve(t.Context(), factory, factory.getter(), target, input{s: map[string]string{engine.FlagCluster: "local"}})
	require.NoError(t, err)
	assert.Equal(t, engine.LocalCluster, resolved.ClusterID)
	require.Len(t, resolved.Pods, 1)
}

func TestResolveServerFallbackChain(t *testing.T) {
	t.Parallel()

	t.Run("primary selector finds pods", func(t *testing.T) {
		t.Parallel()
		local := newFakeClient().withListSelector(podKind.GVR, "cattle-system", "app=rancher", podList("rancher-1"))
		factory := &fakeFactory{local: local}
		target, _ := Lookup("server")

		resolved, err := Resolve(t.Context(), factory, factory.getter(), target, input{})
		require.NoError(t, err)
		assert.Equal(t, "cattle-system", resolved.Namespace)
		require.Len(t, resolved.Pods, 1)
		assert.Equal(t, "rancher-1", resolved.Pods[0].GetName())
	})

	t.Run("falls back to the rancher Deployment's own selector", func(t *testing.T) {
		t.Parallel()
		dep := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "rancher"},
			"spec":     map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "rancher-fallback"}}},
		}}
		local := newFakeClient().
			withObject(deployKind.GVR, "cattle-system", "rancher", dep).
			withListSelector(podKind.GVR, "cattle-system", "app=rancher-fallback", podList("rancher-2"))
		factory := &fakeFactory{local: local}
		target, _ := Lookup("server")

		resolved, err := Resolve(t.Context(), factory, factory.getter(), target, input{})
		require.NoError(t, err)
		require.Len(t, resolved.Pods, 1)
		assert.Equal(t, "rancher-2", resolved.Pods[0].GetName())
	})

	t.Run("degrades to the Docker-install message when nothing matches", func(t *testing.T) {
		t.Parallel()
		local := newFakeClient() // no pods, no rancher Deployment
		factory := &fakeFactory{local: local}
		target, _ := Lookup("server")

		_, err := Resolve(t.Context(), factory, factory.getter(), target, input{})
		require.Error(t, err)
		assert.Equal(t, clierr.CodeNotFound, clierrCode(t, err))
		require.ErrorContains(t, err, "single-node Docker install")
		require.ErrorContains(t, err, "docker logs")
	})
}

func TestResolveKubeAPIAuthPropagatesRealGetError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("connection refused")
	root := (&fakeRoot{}).withError(clusterPath("c-1"), wantErr)
	factory := &fakeFactory{local: newFakeClient(), root: root, perCluster: map[string]engine.ClusterClient{"c-1": newFakeClient()}}
	target, _ := Lookup("kube-api-auth")

	_, err := Resolve(t.Context(), factory, factory.getter(), target, input{s: map[string]string{engine.FlagCluster: "c-1"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr, "a real fetch failure must propagate, not be mistaken for a non-ACE cluster")
}

func TestResolveAgentPropagatesListError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("rbac denied")
	downstream := newFakeClient().withListErr(podKind.GVR, "cattle-system", wantErr)
	factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
	target, _ := Lookup("agent")

	_, err := Resolve(t.Context(), factory, factory.getter(), target, input{s: map[string]string{engine.FlagCluster: "c-1"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

func TestResolveKubeAPIAuth(t *testing.T) {
	t.Parallel()

	aceCluster := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-1"},
		"spec":     map[string]any{"localClusterAuthEndpoint": map[string]any{"enabled": true}},
	}}
	nonACECluster := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-2"},
		"spec":     map[string]any{},
	}}

	t.Run("ACE cluster proceeds to pod fetch", func(t *testing.T) {
		t.Parallel()
		root := (&fakeRoot{}).withCluster(aceCluster)
		downstream := newFakeClient().withListNS(podKind.GVR, "cattle-system", podList("kaa-1"))
		factory := &fakeFactory{local: newFakeClient(), root: root, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
		target, _ := Lookup("kube-api-auth")

		resolved, err := Resolve(t.Context(), factory, factory.getter(), target, input{s: map[string]string{engine.FlagCluster: "c-1"}})
		require.NoError(t, err)
		require.Len(t, resolved.Pods, 1)
	})

	t.Run("non-ACE cluster is NOT_FOUND before any pod fetch", func(t *testing.T) {
		t.Parallel()
		root := (&fakeRoot{}).withCluster(nonACECluster)
		factory := &fakeFactory{local: newFakeClient(), root: root, perCluster: map[string]engine.ClusterClient{"c-2": newFakeClient()}}
		target, _ := Lookup("kube-api-auth")

		_, err := Resolve(t.Context(), factory, factory.getter(), target, input{s: map[string]string{engine.FlagCluster: "c-2"}})
		require.Error(t, err)
		assert.Equal(t, clierr.CodeNotFound, clierrCode(t, err))
		assert.ErrorContains(t, err, "authorized cluster endpoint")
	})
}

func TestResolveFleetAgentClusterDefault(t *testing.T) {
	t.Parallel()
	factory := &fakeFactory{local: newFakeClient()}
	target, _ := Lookup("fleet-agent")

	t.Run("omitted --cluster defaults to local", func(t *testing.T) {
		t.Parallel()
		resolved, err := Resolve(t.Context(), factory, factory.getter(), target, input{})
		require.NoError(t, err)
		assert.Equal(t, engine.LocalCluster, resolved.ClusterID)
	})

	t.Run("--cluster targets the given downstream cluster", func(t *testing.T) {
		t.Parallel()
		f := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": newFakeClient()}}
		resolved, err := Resolve(t.Context(), f, f.getter(), target, input{s: map[string]string{engine.FlagCluster: "c-1"}})
		require.NoError(t, err)
		assert.Equal(t, "c-1", resolved.ClusterID)
	})
}

func TestResolveFleetAgentNamespaceChain(t *testing.T) {
	t.Parallel()

	t.Run("downstream: agentNamespace override via the provisioning-keyed fleetcluster", func(t *testing.T) {
		t.Parallel()
		mgmtCluster := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "c-1"},
			"spec":     map[string]any{},
		}}
		provList := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			{Object: map[string]any{"metadata": map[string]any{"name": "p-1"}, "status": map[string]any{"clusterName": "c-1"}}},
		}}
		fleetCluster := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "p-1"},
			"spec":     map[string]any{"agentNamespace": "custom-fleet-ns"},
		}}
		local := newFakeClient().
			withListNS(provKind.GVR, "fleet-default", provList).
			withObject(fleetKind.GVR, "fleet-default", "p-1", fleetCluster)
		downstream := newFakeClient().withListNS(podKind.GVR, "custom-fleet-ns", podList("fleet-agent-1"))
		root := (&fakeRoot{}).withCluster(mgmtCluster)
		factory := &fakeFactory{local: local, root: root, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
		target, _ := Lookup("fleet-agent")

		resolved, err := Resolve(t.Context(), factory, factory.getter(), target, input{s: map[string]string{engine.FlagCluster: "c-1"}})
		require.NoError(t, err)
		assert.Equal(t, "custom-fleet-ns", resolved.Namespace)
		require.Len(t, resolved.Pods, 1)
	})

	t.Run("downstream: legacy fleet-system fallback when the default namespace has no pods", func(t *testing.T) {
		t.Parallel()
		local := newFakeClient() // no mgmt Cluster, no provisioningclusters, no fleetcluster: every read falls through
		downstream := newFakeClient().withListNS(podKind.GVR, "fleet-system", podList("legacy-agent"))
		factory := &fakeFactory{local: local, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
		target, _ := Lookup("fleet-agent")

		resolved, err := Resolve(t.Context(), factory, factory.getter(), target, input{s: map[string]string{engine.FlagCluster: "c-1"}})
		require.NoError(t, err)
		assert.Equal(t, "fleet-system", resolved.Namespace)
		require.Len(t, resolved.Pods, 1)
	})

	t.Run("local cluster uses the fixed fleetcluster \"local\" in fleet-local, no provisioningcluster lookup", func(t *testing.T) {
		t.Parallel()
		localMgmt := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "local"},
			"spec":     map[string]any{},
		}}
		local := newFakeClient().
			withListNS(podKind.GVR, "cattle-fleet-local-system", podList("local-fleet-agent"))
		factory := &fakeFactory{local: local, root: (&fakeRoot{}).withCluster(localMgmt)}
		target, _ := Lookup("fleet-agent")

		resolved, err := Resolve(t.Context(), factory, factory.getter(), target, input{})
		require.NoError(t, err)
		assert.Equal(t, engine.LocalCluster, resolved.ClusterID)
		assert.Equal(t, "cattle-fleet-local-system", resolved.Namespace)
		require.Len(t, resolved.Pods, 1)
	})

	t.Run("downstream: a failed steve read of the Cluster object falls back to the default workspace", func(t *testing.T) {
		t.Parallel()
		provList := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			{Object: map[string]any{"metadata": map[string]any{"name": "p-1"}, "status": map[string]any{"clusterName": "c-1"}}},
		}}
		fleetCluster := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "p-1"},
			"spec":     map[string]any{"agentNamespace": "custom-fleet-ns"},
		}}
		local := newFakeClient().
			withListNS(provKind.GVR, "fleet-default", provList).
			withObject(fleetKind.GVR, "fleet-default", "p-1", fleetCluster)
		downstream := newFakeClient().withListNS(podKind.GVR, "custom-fleet-ns", podList("fleet-agent-1"))
		root := (&fakeRoot{}).withError(clusterPath("c-1"), rootStatusError(500, clusterPath("c-1")))
		factory := &fakeFactory{local: local, root: root, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
		target, _ := Lookup("fleet-agent")

		resolved, err := Resolve(t.Context(), factory, factory.getter(), target, input{s: map[string]string{engine.FlagCluster: "c-1"}})
		require.NoError(t, err)
		assert.Equal(t, "custom-fleet-ns", resolved.Namespace)
	})
}
