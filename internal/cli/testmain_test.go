package cli

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
)

// TestMain clears the RANCHER_CLI_* variables this package's tests exercise
// as overrides, so a normal interactive shell with them set cannot silently
// change what a test observes.
func TestMain(m *testing.M) {
	cmdtest.UnsetResolutionEnv()
	os.Exit(m.Run())
}
