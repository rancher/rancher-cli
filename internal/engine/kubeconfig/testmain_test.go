package kubeconfig

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
)

// TestMain clears the RANCHER_CLI_* variables the config resolution reads
// ahead of the stored file: the command tests set their own and must not
// inherit an operator's.
func TestMain(m *testing.M) {
	cmdtest.UnsetResolutionEnv()
	os.Exit(m.Run())
}
