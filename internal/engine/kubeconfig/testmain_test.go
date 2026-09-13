package kubeconfig

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
)

// TestMain clears the RANCHER_CLI_* variables the config resolution reads
// ahead of the stored file: the command tests set their own and must not
// inherit an operator's. It also points the config directory at a
// temporary one, so a test that forgets its own cannot reach the
// operator's real one.
func TestMain(m *testing.M) {
	cmdtest.UnsetResolutionEnv()
	cleanup := cmdtest.IsolateConfigDir()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
