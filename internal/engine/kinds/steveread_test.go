package kinds

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rancher gives permission to get, but not to list, the objects of these
// (group, resource) pairs and every object of the groups in
// clusterIndexedGroups: the CRDs Rancher labels as indexed to a cluster. A new row in one of those
// groups is covered by the group.
var grantedByName = [][2]string{
	{"management.cattle.io", "clusters"},
	{"management.cattle.io", "projects"},
	{"management.cattle.io", "users"},
	{"management.cattle.io", "fleetworkspaces"},
	{"", "namespaces"},
}

var clusterIndexedGroups = []string{
	"provisioning.cattle.io",
	"rke.cattle.io",
	"cluster.x-k8s.io",
	MachineConfigGroup,
	"operation.cattle.io",
}

// refusedByTheLocalProxy lists every row that sets SteveReadOnForbidden because
// Rancher's proxy to the local cluster refuses the kind to a caller who cannot
// get the local cluster while Rancher's RBAC allows list and get, each with that
// reason. Such a row is in neither grantedByName nor
// grantedByNameReadThroughKubernetes.
var refusedByTheLocalProxy = map[string]string{
	"managementnode": "Rancher's proxy to the local cluster refuses the kind to a caller who cannot get the local cluster, while Rancher's RBAC allows list and get",
}

// grantedByNameReadThroughKubernetes lists every granted row that does not set
// SteveReadOnForbidden, each with its reason. A row that sets the flag is in
// no list; refusedByTheLocalProxy holds the rows that set it because of
// Rancher's proxy to the local cluster.
var grantedByNameReadThroughKubernetes = map[string]string{
	"capicluster":           "the row finds its version by discovery, and the Steve API returns the version it prefers, which can differ",
	"capimachine":           "the row finds its version by discovery, and the Steve API returns the version it prefers, which can differ",
	"capimachineset":        "the row finds its version by discovery, and the Steve API returns the version it prefers, which can differ",
	"capimachinedeployment": "the row finds its version by discovery, and the Steve API returns the version it prefers, which can differ",
	"amazonec2config":       "the Steve API has not been checked against a real Rancher for a caller who may only get it by name",
	"azureconfig":           "the Steve API has not been checked against a real Rancher for a caller who may only get it by name",
	"digitaloceanconfig":    "the Steve API has not been checked against a real Rancher for a caller who may only get it by name",
	"harvesterconfig":       "the Steve API has not been checked against a real Rancher for a caller who may only get it by name",
	"vmwarevsphereconfig":   "the Steve API has not been checked against a real Rancher for a caller who may only get it by name",
	"user":                  "the Steve API has not been checked against a real Rancher for a caller who may only get it by name",
	"fleetworkspace":        "the Steve API has not been checked against a real Rancher for a caller who may only get it by name",
	"namespace":             "a downstream kind: the Steve read reaches only the local cluster",

	"etcdsnapshotsave":      "Rancher grants a Cluster Owner every verb on it, list included, in the cluster's namespace; its objects name no cluster that the by-name Role indexes",
	"etcdsnapshotrestore":   "Rancher grants a Cluster Owner every verb on it, list included, in the cluster's namespace; its objects name no cluster that the by-name Role indexes",
	"encryptionkeyrotation": "Rancher grants a Cluster Owner every verb on it, list included, in the cluster's namespace; its objects name no cluster that the by-name Role indexes",
	"certificaterotation":   "Rancher grants a Cluster Owner every verb on it, list included, in the cluster's namespace; its objects name no cluster that the by-name Role indexes",
}

// TestSteveReadRowsAreTheRowsRancherGrantsByName asserts that every row for
// which Rancher gives permission to get but not to list either sets
// SteveReadOnForbidden or is listed with the reason it reads through the
// Kubernetes API, never both, that every row in refusedByTheLocalProxy sets it
// and is in no other list, and that no other row sets it or is listed.
func TestSteveReadRowsAreTheRowsRancherGrantsByName(t *testing.T) {
	t.Parallel()
	granted := func(k Kind) bool {
		return slices.Contains(clusterIndexedGroups, k.GVR.Group) ||
			slices.Contains(grantedByName, [2]string{k.GVR.Group, k.GVR.Resource})
	}
	keys := map[string]bool{}
	for _, k := range All() {
		keys[k.Key] = true
		reason, listed := grantedByNameReadThroughKubernetes[k.Key]
		localReason, refused := refusedByTheLocalProxy[k.Key]
		switch {
		case granted(k) && k.SteveReadOnForbidden:
			assert.False(t, listed, "row %s sets SteveReadOnForbidden and is also listed as read through the Kubernetes API", k.Key)
			assert.False(t, refused, "row %s is granted by name and also listed in refusedByTheLocalProxy", k.Key)
		case granted(k):
			assert.False(t, refused, "row %s is granted by name and also listed in refusedByTheLocalProxy", k.Key)
			assert.True(t, listed, "row %s is one Rancher gives permission to get but not to list: set SteveReadOnForbidden, or list it in grantedByNameReadThroughKubernetes with a reason", k.Key)
			assert.NotEmpty(t, reason, "row %s is listed without a reason", k.Key)
		case refused:
			assert.True(t, k.SteveReadOnForbidden, "row %s is listed in refusedByTheLocalProxy but does not set SteveReadOnForbidden", k.Key)
			assert.NotEmpty(t, localReason, "row %s is listed in refusedByTheLocalProxy without a reason", k.Key)
			assert.False(t, listed, "row %s is listed as read through the Kubernetes API and as refused by Rancher's proxy to the local cluster", k.Key)
		default:
			assert.False(t, k.SteveReadOnForbidden, "row %s sets SteveReadOnForbidden but Rancher gives no permission to get it by name", k.Key)
			assert.False(t, listed, "row %s is listed in grantedByNameReadThroughKubernetes but Rancher gives no permission to get it by name", k.Key)
		}
	}
	for key := range grantedByNameReadThroughKubernetes {
		assert.True(t, keys[key], "grantedByNameReadThroughKubernetes lists %q, which is no row", key)
	}
	for key := range refusedByTheLocalProxy {
		assert.True(t, keys[key], "refusedByTheLocalProxy lists %q, which is no row", key)
	}
	var readThroughSteve []string
	for _, k := range All() {
		if k.SteveReadOnForbidden {
			readThroughSteve = append(readThroughSteve, k.Key)
		}
	}
	slices.Sort(readThroughSteve)
	require.Equal(t, []string{"etcdsnapshot", "managementcluster", "managementnode", "project", "provisioningcluster"}, readThroughSteve)
}

// TestSteveTypeSpellsTheTypeAsSteveDoesInAPath asserts the type of a row with
// a group is group.resource and of a row with no group is the resource.
func TestSteveTypeSpellsTheTypeAsSteveDoesInAPath(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]string{
		"provisioningcluster": "provisioning.cattle.io.clusters",
		"etcdsnapshot":        "rke.cattle.io.etcdsnapshots",
		"namespace":           "namespaces",
	} {
		k, ok := ByKey(key)
		require.True(t, ok, key)
		assert.Equal(t, want, k.SteveType(), key)
	}
}
