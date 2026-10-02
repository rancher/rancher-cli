package cli

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
)

// TestMain clears the RANCHER_CLI_* variables this package's tests exercise
// as overrides, so a normal interactive shell with them set cannot silently
// change what a test observes, and points the config directory at a
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
