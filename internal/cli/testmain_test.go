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
// operator's real one.
func TestMain(m *testing.M) {
	cmdtest.UnsetResolutionEnv()
	cleanup := cmdtest.IsolateConfigDir()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
