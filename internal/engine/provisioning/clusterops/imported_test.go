package clusterops

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func managementCluster(name, driver, label string) map[string]any {
	meta := map[string]any{"name": name}
	if label != "" {
		meta["labels"] = map[string]any{"provider.cattle.io": label}
	}
	status := map[string]any{}
	if driver != "" {
		status["driver"] = driver
	}
	return map[string]any{"metadata": meta, "status": status}
}

// TestImportedDistribution asserts the distribution comes from the driver,
// comes from the label only for a driver that is neither rke2 nor k3s and only
// when asked, and is never returned for the local cluster.
func TestImportedDistribution(t *testing.T) {
	t.Parallel()
	drivers := []string{"", "imported", "k3s", "rke2", "local", "rancherKubernetesEngine", "EKS", "AKS", "GKE", "Alibaba", "k3os", "rancherd"}
	labels := []string{"", "rke2", "k3s", "other"}
	for _, driver := range drivers {
		for _, label := range labels {
			for _, byLabel := range []bool{false, true} {
				want, wantOK := "", false
				switch {
				case driver == "rke2" || driver == "k3s":
					want, wantOK = driver, true
				case byLabel && (label == "rke2" || label == "k3s"):
					want, wantOK = label, true
				}
				got, ok := ImportedDistribution(managementCluster("c-m-abc12", driver, label), byLabel)
				assert.Equal(t, want, got, "driver %q label %q byLabel %v", driver, label, byLabel)
				assert.Equal(t, wantOK, ok, "driver %q label %q byLabel %v", driver, label, byLabel)
			}
		}
	}
}

// TestImportedDistributionKeepsTheDriverOverTheLabel asserts a driver
// rke2 with the label k3s is rke2, and the reverse.
func TestImportedDistributionKeepsTheDriverOverTheLabel(t *testing.T) {
	t.Parallel()
	got, ok := ImportedDistribution(managementCluster("c-m-abc12", "rke2", "k3s"), true)
	assert.True(t, ok)
	assert.Equal(t, "rke2", got)
	got, ok = ImportedDistribution(managementCluster("c-m-abc12", "k3s", "rke2"), true)
	assert.True(t, ok)
	assert.Equal(t, "k3s", got)
}

// TestImportedDistributionRefusesLocal asserts the local cluster is not an
// imported cluster whatever its driver and label say.
func TestImportedDistributionRefusesLocal(t *testing.T) {
	t.Parallel()
	for _, byLabel := range []bool{false, true} {
		_, ok := ImportedDistribution(managementCluster("local", "k3s", "k3s"), byLabel)
		assert.False(t, ok)
	}
}
