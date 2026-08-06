package cli

import (
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// buildManifest constructs the full schema manifest directly from the wired
// registry without building the urfave CLI tree.
//
// TestSchemaGroupsMatchRootAssembly is the drift tripwire: it compares
// BuildRoot's real command assembly against this helper's manifest, so a
// divergence in BuildRoot's wiring surfaces as a test failure there. The
// golden tests drive root.Run via --all; this helper feeds the
// manifest-invariant tests.
func buildManifest(version string) (runtime.Manifest, error) {
	reg, _, _, _, err := buildWiredRegistry(version, nil)
	if err != nil {
		return runtime.Manifest{}, err
	}
	return reg.Schema(version, config.MinRancherVersion), nil
}
