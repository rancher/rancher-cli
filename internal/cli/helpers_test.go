package cli

import (
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// buildManifest builds the wired registry (buildWiredRegistry Adapts the
// schema and completion commands, among others) and reads only the registry
// for the manifest; the adapted CLI commands are discarded.
//
// TestSchemaGroupsMatchRootAssembly is the drift tripwire: it compares
// BuildRoot's real command assembly against this helper's manifest, so a
// divergence in BuildRoot's wiring surfaces as a test failure there. The
// golden tests drive root.Run via --all; this helper feeds the
// manifest-invariant tests.
func buildManifest(version string) (runtime.Manifest, error) {
	wr, err := buildWiredRegistry(version, nil)
	if err != nil {
		return runtime.Manifest{}, err
	}
	return wr.reg.Schema(version, config.MinRancherVersion), nil
}
