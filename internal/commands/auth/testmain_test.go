package auth

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
)

// TestMain fails the package when a test leaves a new file in the
// package directory.
func TestMain(m *testing.M) {
	cmdtest.UnsetResolutionEnv()
	cleanup := cmdtest.IsolateConfigDir()
	since := cmdtest.NewFiles()
	code := cmdtest.FailOnNewFiles(m.Run(), since, os.Stderr)
	cleanup()
	os.Exit(code)
}
