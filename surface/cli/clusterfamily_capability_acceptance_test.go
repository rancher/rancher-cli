package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/provisioning"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// This file is the cluster-family acceptance coverage for the capability
// gate on a STATIC Capability requirement authored directly on a bespoke
// command's Descriptor, with no backing kind row underneath - the case the
// axis was built for but the oidcclient acceptance coverage (a kind-row
// Capability) never exercises. It asserts the same four detector outcomes live
// through the pipeline on `cluster set-autoscaling`.

// clusterFamilyAcceptanceServer serves /rancherversion, the
// cluster-autoscaling Feature GET, and the target provisioning Cluster's
// GET/PUT route, counting target-resource hits separately from the
// capability-detector requests.
func clusterFamilyAcceptanceServer(t *testing.T, rancherVersion, featureBody string, featureStatus int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var targetHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/rancherversion", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rancherVersion))
	})
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/features/cluster-autoscaling", func(w http.ResponseWriter, r *http.Request) {
		if featureBody == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(featureStatus)
		_, _ = w.Write([]byte(featureBody))
	})
	mux.HandleFunc("/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters/my-cluster", func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		grWriteJSON(w, map[string]any{
			"apiVersion": "provisioning.cattle.io/v1",
			"kind":       "Cluster",
			"metadata":   map[string]any{"name": "my-cluster", "namespace": "fleet-default", "resourceVersion": "1"},
			"spec": map[string]any{"rkeConfig": map[string]any{"machinePools": []map[string]any{
				{"name": "pool1", "quantity": int64(2), "workerRole": true},
			}}},
		})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, &targetHits
}

// clusterAutoscalingFeatureObj builds a minimal, wire-realistic Feature JSON
// body for "cluster-autoscaling" - same shape oidcFeatureObj builds for
// "oidc-provider".
func clusterAutoscalingFeatureObj(prime, defaultVal bool) string {
	return fmt.Sprintf(`{"kind":"Feature","apiVersion":"management.cattle.io/v3",
		"metadata":{"name":"cluster-autoscaling"},
		"status":{"prime":%t,"default":%t}}`, prime, defaultVal)
}

// TestClusterFamilyCapabilityFourOutcomesLive asserts the four detector
// outcomes live through runtime.Run on a bespoke command carrying a static
// Capability literal (cluster set-autoscaling): proceed and unknown both
// reach the target GET, disabled and PRIME_REQUIRED both refuse before any
// target request - the same shape TestOIDCClientCapabilityFourOutcomesLive
// asserts for a kind-row Capability.
func TestClusterFamilyCapabilityFourOutcomesLive(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	args := []string{"cluster", "set-autoscaling", "my-cluster", "--pool", "pool1", "--min", "1", "--max", "3", "--dry-run", "-o", "json"}

	t.Run("proceed: feature enabled", func(t *testing.T) {
		srv, hits := clusterFamilyAcceptanceServer(t,
			`{"Version":"v2.16.0","RancherPrime":"true"}`,
			clusterAutoscalingFeatureObj(true, true), http.StatusOK)
		dir := grConfigDir(t, srv)
		_, stderr, err := grRun(t, root, dir, args...)
		require.NoError(t, err, "stderr: %s", stderr)
		assert.Equal(t, int32(2), hits.Load(), "proceed must reach the target GET and the dry-run Update")
	})

	t.Run("unknown: feature absent never blocks", func(t *testing.T) {
		srv, hits := clusterFamilyAcceptanceServer(t,
			`{"Version":"v2.16.0","RancherPrime":"true"}`,
			"", http.StatusNotFound)
		dir := grConfigDir(t, srv)
		_, stderr, err := grRun(t, root, dir, args...)
		require.NoError(t, err, "stderr: %s", stderr)
		assert.Equal(t, int32(2), hits.Load(), "unknown outcome must still reach the target GET and the dry-run Update")
	})

	t.Run("disabled: prime edition with the feature off", func(t *testing.T) {
		srv, hits := clusterFamilyAcceptanceServer(t,
			`{"Version":"v2.16.0","RancherPrime":"true"}`,
			clusterAutoscalingFeatureObj(false, false), http.StatusOK)
		dir := grConfigDir(t, srv)
		stdout, stderr, err := grRun(t, root, dir, args...)
		require.Error(t, err)
		env := oidcErrEnvelope(t, stdout, stderr)
		assert.Equal(t, clierr.CodeValidation, env.Data.Error.Code)
		assert.Contains(t, env.Data.Error.Message, "cluster-autoscaling is disabled; enable it on the connected server")
		assert.Zero(t, hits.Load(), "disabled must make zero target requests")
	})

	t.Run("prime required: community edition, prime-classified feature enabled only by its status.default", func(t *testing.T) {
		// The live-captured community shape: a community server's cluster-autoscaling
		// Feature reads enabled=true purely from status.default (no
		// operator ever set spec.value) - Rancher's own Feature.Enabled()
		// still disables it server-side on a non-Prime build, so this must
		// resolve PRIME_REQUIRED, not proceed.
		srv, hits := clusterFamilyAcceptanceServer(t,
			`{"Version":"v2.16.0","RancherPrime":"false"}`,
			clusterAutoscalingFeatureObj(true, true), http.StatusOK)
		dir := grConfigDir(t, srv)
		stdout, stderr, err := grRun(t, root, dir, args...)
		require.Error(t, err)
		env := oidcErrEnvelope(t, stdout, stderr)
		assert.Equal(t, clierr.CodePrimeRequired, env.Data.Error.Code)
		assert.Zero(t, hits.Load(), "PRIME_REQUIRED must make zero target requests")
	})

	t.Run("prime required: community edition, prime-classified feature off", func(t *testing.T) {
		srv, hits := clusterFamilyAcceptanceServer(t,
			`{"Version":"v2.16.0","RancherPrime":"false"}`,
			clusterAutoscalingFeatureObj(true, false), http.StatusOK)
		dir := grConfigDir(t, srv)
		stdout, stderr, err := grRun(t, root, dir, args...)
		require.Error(t, err)
		env := oidcErrEnvelope(t, stdout, stderr)
		assert.Equal(t, clierr.CodePrimeRequired, env.Data.Error.Code)
		var details struct {
			Feature string `json:"feature"`
		}
		require.NoError(t, json.Unmarshal(env.Data.Error.Details, &details))
		assert.Equal(t, "cluster-autoscaling", details.Feature)
		assert.Zero(t, hits.Load(), "PRIME_REQUIRED must make zero target requests")
	})
}

// TestClusterScaleCarriesNoCapability asserts the family's proof-of-both-cases
// requirement: `cluster scale` - the community verb sitting beside the four
// Prime-gated ones in the SAME group - carries no Capability requirement at
// all, so it never consults the detector regardless of server edition.
func TestClusterScaleCarriesNoCapability(t *testing.T) {
	d := provisioning.NewScale(nil).Describe()
	assert.False(t, d.Capability.IsSet(), "cluster scale must carry no Capability requirement")
}
