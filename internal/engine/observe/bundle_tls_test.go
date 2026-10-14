package observe

import (
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// certificateFailure is the error a certificate failure of the server
// returns, with the remedy command a message carries opened by the marker.
func certificateFailure() error {
	return clierr.New(clierr.CodeTLSTrust, "the server presented a certificate signed by an unknown authority; "+
		"save it for this entry with: "+clierr.Rancher+" config update-server 'prod' --cacert <file>")
}

const plainRemedy = "rancher config update-server 'prod' --cacert <file>"

// TestSupportBundleRootClientFailureLeavesNoRemedyMarkerInTheManifest
// asserts a certificate failure of the root client, recorded as an item's
// error status, reaches the archive manifest as plain text: the archive is
// not rendered by an invocation that expands remedy markers. Mutation: drop
// the marker removal in errorStatus.
func TestSupportBundleRootClientFailureLeavesNoRemedyMarkerInTheManifest(t *testing.T) {
	t.Parallel()
	factory, _ := ownerFactory(newFakeClient())
	factory.root.withError(clusterPath("c-abc"), certificateFailure())

	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	_, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "agent", flagClusters: "prod"}})
	require.NoError(t, err)

	status := findItem(t, manifestFrom(t, path), "cluster.json", "c-abc").Status
	assert.Contains(t, status, plainRemedy)
	assert.NotContains(t, status, clierr.RemedyMarker)
	assert.NotContains(t, readArchive(t, path)["manifest.json"], clierr.RemedyMarker)
}

// TestSupportBundleClusterClientLogFailureLeavesNoRemedyMarkerInTheManifest
// is the same assertion for a certificate failure of the cluster client
// reading a container's log, which the target's status names. Mutation:
// drop the marker removal in logOutcome.status.
func TestSupportBundleClusterClientLogFailureLeavesNoRemedyMarkerInTheManifest(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().withLogErr("main", certificateFailure())
	_, path := bundleWithAgentPod(t, downstream)

	status := findItem(t, manifestFrom(t, path), "agent", "c-1").Status
	assert.Contains(t, status, plainRemedy)
	assert.NotContains(t, status, clierr.RemedyMarker)
	assert.NotContains(t, readArchive(t, path)["manifest.json"], clierr.RemedyMarker)
}
