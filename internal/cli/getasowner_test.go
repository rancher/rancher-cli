package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ownerK8sProvisioning = "/k8s/clusters/local/apis/provisioning.cattle.io/v1"
	ownerK8sEtcd         = "/k8s/clusters/local/apis/rke.cattle.io/v1"
	ownerSteveClusters   = "/v1/provisioning.cattle.io.clusters/fleet-default"
	ownerSteveSnapshots  = "/v1/rke.cattle.io.etcdsnapshots/fleet-default"
	ownerSnapshotName    = "prod-etcd-snapshot-node-1-1794614404-abcde"
)

// ownerProvisioningCluster is a provisioning cluster as Steve returns it in
// fleet-default.
func ownerProvisioningCluster() map[string]any {
	return map[string]any{
		"id": "fleet-default/prod", "_id": "fleet-default/prod", "type": "provisioning.cattle.io.cluster",
		"links":      map[string]any{"self": "https://rancher.invalid/v1/provisioning.cattle.io.clusters/fleet-default/prod"},
		"apiVersion": "provisioning.cattle.io/v1", "kind": "Cluster",
		"metadata": map[string]any{
			"name": "prod", "namespace": "fleet-default", "resourceVersion": "10",
			"fields": []any{"prod", true}, "state": map[string]any{"name": "active"}, "relationships": []any{},
		},
		"spec": map[string]any{"kubernetesVersion": "v1.33.1+rke2r1"},
		"status": map[string]any{
			"clusterName": "c-m-abc", "ready": true,
			"conditions": []any{map[string]any{"type": "Ready", "status": "True", "error": false, "transitioning": false}},
		},
	}
}

// ownerEtcdSnapshot is an etcd snapshot as Steve returns it in fleet-default.
func ownerEtcdSnapshot() map[string]any {
	return map[string]any{
		"id": "fleet-default/" + ownerSnapshotName, "_id": "fleet-default/" + ownerSnapshotName, "type": "rke.cattle.io.etcdsnapshot",
		"links":      map[string]any{"self": "https://rancher.invalid/v1/rke.cattle.io.etcdsnapshots/fleet-default/" + ownerSnapshotName},
		"apiVersion": "rke.cattle.io/v1", "kind": "ETCDSnapshot",
		"metadata": map[string]any{
			"name": ownerSnapshotName, "namespace": "fleet-default", "resourceVersion": "11",
			"fields": []any{ownerSnapshotName}, "state": map[string]any{"name": "active"}, "relationships": nil,
		},
		"spec":         map[string]any{"clusterName": "prod"},
		"snapshotFile": map[string]any{"name": "etcd-snapshot-node-1-1794614404", "nodeName": "node-1", "size": 1048576, "status": "successful"},
	}
}

// newOwnerServer is a Rancher for a Cluster Owner with no other role: the
// Kubernetes API responds with 403 to every read of the provisioning clusters
// and etcd snapshots, and Steve lists the one cluster and the one snapshot the
// caller may get. With refuseSteve Steve responds with 403 too. The returned
// server records every request path.
func newOwnerServer(t *testing.T, refuseSteve bool) *memberServer {
	t.Helper()
	s := &memberServer{}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.got = append(s.got, r.URL.Path)
		s.mu.Unlock()
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, ownerK8sProvisioning), strings.HasPrefix(p, ownerK8sEtcd):
			grWriteStatus(w, http.StatusForbidden, "Forbidden", "forbidden for this user")
		case refuseSteve && strings.HasPrefix(p, "/v1/"):
			grWriteStatus(w, http.StatusForbidden, "Forbidden", "the Steve API refuses the read")
		case p == ownerSteveClusters:
			grWriteJSON(w, steveCollectionOf(ownerProvisioningCluster()))
		case p == ownerSteveSnapshots:
			grWriteJSON(w, steveCollectionOf(ownerEtcdSnapshot()))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// envelopeKindAndData decodes the kind and data of a JSON envelope.
func envelopeKindAndData(t *testing.T, stdout []byte) (string, any) {
	t.Helper()
	var env struct {
		Kind string `json:"kind"`
		Data any    `json:"data"`
	}
	require.NoError(t, json.Unmarshal(stdout, &env), "stdout: %s", stdout)
	return env.Kind, env.Data
}

// TestGetProvisioningClustersAsClusterOwner asserts that when the Kubernetes
// API responds to the list with 403, get provisioning-clusters exits 0 with
// the ClusterList the Steve API lists for the caller, in the Kubernetes shape,
// and that when Steve refuses too the run ends with exit 6 and the Kubernetes
// API's error.
func TestGetProvisioningClustersAsClusterOwner(t *testing.T) {
	s := newOwnerServer(t, false)
	stdout, res, stderr, _ := getAs(t, s, "get", "provisioning-clusters", "-o", "json")
	require.Nil(t, res.err, stderr)
	assert.Equal(t, 0, res.code)
	kind, data := envelopeKindAndData(t, stdout)
	assert.Equal(t, "ClusterList", kind)
	assert.Equal(t, []string{"fleet-default/prod"}, dataNames(t, data))
	for _, item := range data.([]any) {
		assertNoSteveFields(t, item.(map[string]any))
	}
	assert.Equal(t, []string{ownerK8sProvisioning + "/namespaces/fleet-default/clusters", ownerSteveClusters}, s.requested())

	refused := newOwnerServer(t, true)
	_, res, stderr, _ = getAs(t, refused, "get", "provisioning-clusters", "-o", "json")
	require.NotNil(t, res.err)
	assert.Equal(t, 6, res.code)
	ce := renderedError(t, "json", stderr)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Contains(t, ce.Message, "forbidden for this user", "the Kubernetes API's own message")
}

// TestGetEtcdSnapshotsAsClusterOwner asserts the same for get etcd-snapshots:
// the ETCDSnapshotList the Steve API lists for the caller, in the Kubernetes
// shape, and the Kubernetes API's error with exit 6 when Steve refuses too.
func TestGetEtcdSnapshotsAsClusterOwner(t *testing.T) {
	s := newOwnerServer(t, false)
	stdout, res, stderr, _ := getAs(t, s, "get", "etcd-snapshots", "-o", "json")
	require.Nil(t, res.err, stderr)
	assert.Equal(t, 0, res.code)
	kind, data := envelopeKindAndData(t, stdout)
	assert.Equal(t, "ETCDSnapshotList", kind)
	assert.Equal(t, []string{"fleet-default/" + ownerSnapshotName}, dataNames(t, data))
	for _, item := range data.([]any) {
		assertNoSteveFields(t, item.(map[string]any))
	}
	assert.Equal(t, []string{ownerK8sEtcd + "/namespaces/fleet-default/etcdsnapshots", ownerSteveSnapshots}, s.requested())

	refused := newOwnerServer(t, true)
	_, res, stderr, _ = getAs(t, refused, "get", "etcd-snapshots", "-o", "json")
	require.NotNil(t, res.err)
	assert.Equal(t, 6, res.code)
	ce := renderedError(t, "json", stderr)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Contains(t, ce.Message, "forbidden for this user", "the Kubernetes API's own message")
}

// TestClusterScaleAndGetEtcdSnapshotTabOfferAnOwnersObjects asserts that
// completing the cluster argument of cluster scale and the name argument of get
// etcd-snapshot, for a Cluster Owner, sends one Steve request and no Kubernetes
// request, and offers the names Steve lists.
func TestClusterScaleAndGetEtcdSnapshotTabOfferAnOwnersObjects(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case ownerSteveClusters:
			grWriteJSON(w, steveCollectionOf(ownerProvisioningCluster()))
		case ownerSteveSnapshots:
			grWriteJSON(w, steveCollectionOf(ownerEtcdSnapshot()))
		case ownerK8sProvisioning + "/namespaces/fleet-default/clusters", ownerK8sEtcd + "/namespaces/fleet-default/etcdsnapshots":
			grWriteStatus(w, http.StatusForbidden, "Forbidden", "forbidden for this user")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	completionFixture(t, srv.URL, true)

	for _, tc := range []struct {
		words []string
		want  []string
		path  string
	}{
		{[]string{"cluster", "scale"}, []string{"prod"}, ownerSteveClusters},
		{[]string{"get", "etcd-snapshot"}, []string{ownerSnapshotName}, ownerSteveSnapshots},
	} {
		mu.Lock()
		hits = nil
		mu.Unlock()
		got := runCompletion(t, tc.words, "")
		assert.Equal(t, tc.want, got, tc.words)
		mu.Lock()
		assert.Equal(t, []string{tc.path}, slices.Clone(hits), "%v: one Steve request and no Kubernetes request", tc.words)
		mu.Unlock()
	}
}
