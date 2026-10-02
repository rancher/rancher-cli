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
// operator's real one. A test that leaves a new file in the package
// directory fails the package.
func TestMain(m *testing.M) {
	cmdtest.UnsetResolutionEnv()
	cleanup := cmdtest.IsolateConfigDir()
	since := cmdtest.NewFiles()
	code := cmdtest.FailOnNewFiles(m.Run(), since, os.Stderr)
	cleanup()
	os.Exit(code)
}
