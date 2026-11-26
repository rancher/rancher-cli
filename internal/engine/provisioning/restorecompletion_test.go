package provisioning

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	completionClusterPath   = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters/prod"
	completionSnapshotsPath = "/v1/rke.cattle.io.etcdsnapshots/fleet-default"
	completionSnapshotPath  = "/k8s/clusters/local/apis/rke.cattle.io/v1/namespaces/fleet-default/etcdsnapshots/s1"
)

// TestRestoreCompletionAgreesWithThePlan asserts, for every row of the table
// of the snapshot checks, that --snapshot offers the etcd snapshot object
// exactly when the Plan accepts it with the restore mode the line gives (typed,
// or its default), that --restore-mode offers exactly the modes the Plan
// accepts, and that every mode offered is one of the values of the flag.
// Readiness and running operations are not compared: completion checks neither.
func TestRestoreCompletionAgreesWithThePlan(t *testing.T) {
	t.Parallel()
	var enum []string
	for _, flag := range (&restoreEtcdSnapshotCmd{}).Describe().Flags {
		if flag.Name == restoreModeFlag {
			enum = flag.Enum
		}
	}
	require.NotEmpty(t, enum)
	for _, tc := range restoreRows() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			snap := restorableSnapshot(t, "s1")
			tc.change(t, snap)

			planAccepts := func(mode string) bool {
				f := restoreFixture(t, map[string]any{})
				f.local.put(etcdSnapshotRow.GVR, "fleet-default", "s1", snap)
				in := restoreInput("s1")
				if mode != "" {
					in = restoreInput("s1", restoreModeFlag, mode)
				}
				_, _, err := f.restorePlan(t, in)
				return err == nil
			}

			prov, err := json.Marshal(restoreProvisioning(map[string]any{}).Object)
			require.NoError(t, err)
			one, err := json.Marshal(snap.Object)
			require.NoError(t, err)
			page := `{"data":[` + string(one) + `]}`
			s := completiontest.New(t, map[string]string{completionClusterPath: string(prov), completionSnapshotsPath: page, completionSnapshotPath: string(one)})
			src := (&restoreEtcdSnapshotCmd{}).LiveSource()

			snapshotReq := completion.Request{Flag: restoreSnapshotFlag, Positionals: []string{"prod"}}
			if tc.mode != "" {
				snapshotReq.Tail = []string{"--" + restoreModeFlag, tc.mode}
			}
			names := src.Complete(t.Context(), s.Conn(2), snapshotReq).Names
			if planAccepts(tc.mode) {
				assert.Equal(t, []string{"s1"}, names, "the Plan accepts the snapshot")
			} else {
				assert.Empty(t, names, "the Plan refuses the snapshot")
			}

			var want []string
			for _, mode := range []string{clusterops.RestoreModeNone, clusterops.RestoreModeKubernetesVersion, clusterops.RestoreModeAll} {
				if planAccepts(mode) {
					want = append(want, mode)
				}
			}
			modes := src.Complete(t.Context(), s.Conn(2), completion.Request{Flag: restoreModeFlag, Positionals: []string{"prod"}, Tail: []string{"--" + restoreSnapshotFlag, "s1"}}).Names
			assert.Equal(t, want, modes, "the modes the Plan accepts")
			for _, mode := range modes {
				assert.True(t, slices.Contains(enum, mode), "%s is a value of the flag", mode)
			}
		})
	}
}
